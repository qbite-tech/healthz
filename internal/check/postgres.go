package check

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/runeQ/healthz/internal/config"
)

// maxDetailBytes bounds the query's explanation stored in history and sent by email.
const maxDetailBytes = 500

// PostgresChecker runs a query that reports health as its first column.
//
// The query must return exactly one row: a boolean healthy column and an
// optional text column explaining the result. It runs in a read-only
// transaction with a statement timeout, so a check can never write data.
type PostgresChecker struct {
	lookupEnv func(string) (string, bool)
}

func NewPostgresChecker() *PostgresChecker {
	return &PostgresChecker{lookupEnv: os.LookupEnv}
}

func (c *PostgresChecker) Check(parent context.Context, endpoint config.EndpointConfig) Result {
	ctx, cancel := context.WithTimeout(parent, endpoint.Timeout.Duration)
	defer cancel()

	started := time.Now()
	failed := func(err error) Result {
		return Result{Duration: time.Since(started), Err: err}
	}
	dsn, ok := c.lookupEnv(endpoint.DSNEnv)
	if !ok || strings.TrimSpace(dsn) == "" {
		return failed(fmt.Errorf("environment variable %s is not set", endpoint.DSNEnv))
	}
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		// Parse errors can echo the connection string, so never include them.
		return failed(fmt.Errorf("environment variable %s does not contain a valid PostgreSQL DSN", endpoint.DSNEnv))
	}
	// Transaction-pooled PgBouncer cannot keep named prepared statements between
	// transactions. Keep an explicit default_query_exec_mode from the DSN.
	if connConfig.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement {
		connConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return failed(fmt.Errorf("connect: %w", err))
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = conn.Close(closeCtx)
	}()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return failed(fmt.Errorf("begin read-only transaction: %w", err))
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// SET LOCAL ends with the transaction, so no setting leaks into a pooled session.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", endpoint.Timeout.Milliseconds())); err != nil {
		return failed(fmt.Errorf("set statement timeout: %w", err))
	}
	rows, err := tx.Query(ctx, endpoint.Query)
	if err != nil {
		return failed(fmt.Errorf("query: %w", err))
	}
	healthy, detail, err := readHealthRow(rows)
	if err != nil {
		return failed(err)
	}
	if !healthy {
		if detail == "" {
			detail = "query reported unhealthy"
		}
		return failed(errors.New(detail))
	}
	return Result{Healthy: true, Duration: time.Since(started)}
}

// healthRows is the subset of pgx.Rows used to read a check result.
type healthRows interface {
	Next() bool
	Values() ([]any, error)
	Err() error
	Close()
}

func readHealthRow(rows healthRows) (bool, string, error) {
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, "", fmt.Errorf("query: %w", err)
		}
		return false, "", errors.New("query returned no rows; expected exactly one")
	}
	values, err := rows.Values()
	if err != nil {
		return false, "", fmt.Errorf("read query result: %w", err)
	}
	if len(values) < 1 || len(values) > 2 {
		return false, "", fmt.Errorf("query returned %d columns; expected healthy boolean and optional detail text", len(values))
	}
	healthy, ok := values[0].(bool)
	if values[0] != nil && !ok {
		return false, "", fmt.Errorf("query's first column is %T; expected boolean", values[0])
	}
	var detail string
	if len(values) == 2 && values[1] != nil {
		text, ok := values[1].(string)
		if !ok {
			return false, "", fmt.Errorf("query's second column is %T; expected text", values[1])
		}
		detail = truncate(strings.TrimSpace(text), maxDetailBytes)
	}
	if rows.Next() {
		return false, "", errors.New("query returned more than one row; expected exactly one")
	}
	if err := rows.Err(); err != nil {
		return false, "", fmt.Errorf("query: %w", err)
	}
	if values[0] == nil && detail == "" {
		detail = "query returned NULL for healthy"
	}
	return healthy, detail, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "…"
}
