package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/qbite-tech/healthz/internal/check"
	"github.com/qbite-tech/healthz/internal/config"
	_ "modernc.org/sqlite"
)

const schemaVersion = 4

type EndpointStatus struct {
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Known               bool       `json:"known"`
	Healthy             bool       `json:"healthy"`
	LastChecked         time.Time  `json:"last_checked,omitempty"`
	LastStatusCode      int        `json:"last_status_code,omitempty"`
	LastLatencyMillis   int64      `json:"last_latency_ms"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error,omitempty"`
	OutageStartedAt     *time.Time `json:"outage_started_at,omitempty"`
	AlertActive         bool       `json:"alert_active"`
	LastAlertSentAt     *time.Time `json:"last_alert_sent_at,omitempty"`
	LastRecoveredAt     *time.Time `json:"last_recovered_at,omitempty"`
	Flapping            bool       `json:"flapping"`
	LastFlapAlertAt     *time.Time `json:"last_flap_alert_at,omitempty"`
	LastPingAt          *time.Time `json:"last_ping_at,omitempty"`
	LastPingStartAt     *time.Time `json:"last_ping_start_at,omitempty"`
	LastPingFailAt      *time.Time `json:"last_ping_fail_at,omitempty"`
}

type DigestStats struct {
	Checks       int `json:"checks"`
	PassedChecks int `json:"passed_checks"`
	FailedChecks int `json:"failed_checks"`
}

type DigestEndpoint struct {
	Status EndpointStatus
	Stats  DigestStats
}

type DigestSnapshot struct {
	From        time.Time
	To          time.Time
	StartedAt   time.Time
	LastCheckID int64
	Endpoints   []DigestEndpoint
}

type HistorySummary struct {
	Endpoint            string    `json:"endpoint"`
	From                time.Time `json:"from"`
	To                  time.Time `json:"to"`
	Checks              int       `json:"checks"`
	PassedChecks        int       `json:"passed_checks"`
	FailedChecks        int       `json:"failed_checks"`
	AvailabilityPercent *float64  `json:"availability_percent,omitempty"`
	AverageLatencyMS    *float64  `json:"average_latency_ms,omitempty"`
	MinimumLatencyMS    *float64  `json:"minimum_latency_ms,omitempty"`
	MaximumLatencyMS    *float64  `json:"maximum_latency_ms,omitempty"`
}

type StatusStore struct {
	db            *sql.DB
	startedAt     time.Time
	retentionDays int
}

func OpenStatusStore(path string, endpoints []config.EndpointConfig, retentionDays int) (*StatusStore, error) {
	store, err := openStore(path, retentionDays)
	if err != nil {
		return nil, err
	}
	if err := store.migrateSchema(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.syncEndpoints(context.Background(), endpoints); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// OpenTokenStore opens the shared database and migrates its schema without
// reconciling endpoint rows. It is intended for local token administration.
func OpenTokenStore(path string) (*StatusStore, error) {
	store, err := openStore(path, 30)
	if err != nil {
		return nil, err
	}
	if err := store.migrateSchema(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func openStore(path string, retentionDays int) (*StatusStore, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create storage directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	// A single connection avoids lock contention while WAL still gives crash-safe,
	// transactional persistence for this single-process service.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &StatusStore{db: db, startedAt: time.Now(), retentionDays: retentionDays}
	if err := store.configureSQLite(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("secure SQLite database permissions: %w", err)
		}
	}
	return store, nil
}

// NewStatusStore provides an isolated in-memory SQLite store for package users
// and tests. Production code should use OpenStatusStore with a durable path.
func NewStatusStore(endpoints []config.EndpointConfig) *StatusStore {
	store, err := OpenStatusStore(":memory:", endpoints, 30)
	if err != nil {
		panic(err)
	}
	return store
}

func (s *StatusStore) configureSQLite(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite: %w", err)
		}
	}
	return nil
}

func (s *StatusStore) migrateSchema(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read SQLite schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("SQLite schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite migration: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS endpoints (
			name TEXT PRIMARY KEY,
			url TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			known INTEGER NOT NULL DEFAULT 0,
			healthy INTEGER NOT NULL DEFAULT 0,
			last_checked_ns INTEGER,
			last_status_code INTEGER NOT NULL DEFAULT 0,
			last_latency_ns INTEGER NOT NULL DEFAULT 0,
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			outage_started_ns INTEGER,
			alert_active INTEGER NOT NULL DEFAULT 0,
			last_alert_sent_ns INTEGER,
			last_recovered_ns INTEGER,
			flapping_active INTEGER NOT NULL DEFAULT 0,
			last_flap_alert_sent_ns INTEGER,
			ping_baseline_ns INTEGER,
			last_ping_success_ns INTEGER,
			last_ping_start_ns INTEGER,
			last_ping_fail_ns INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS checks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			endpoint_name TEXT NOT NULL REFERENCES endpoints(name),
			endpoint_url TEXT NOT NULL,
			checked_at_ns INTEGER NOT NULL,
			healthy INTEGER NOT NULL,
			status_code INTEGER NOT NULL,
			latency_ns INTEGER NOT NULL,
			error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS checks_endpoint_time_idx ON checks(endpoint_name, checked_at_ns)`,
		`CREATE INDEX IF NOT EXISTS checks_time_idx ON checks(checked_at_ns)`,
		`CREATE TABLE IF NOT EXISTS state_transitions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			endpoint_name TEXT NOT NULL REFERENCES endpoints(name),
			changed_at_ns INTEGER NOT NULL,
			healthy INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS state_transitions_endpoint_time_idx
			ON state_transitions(endpoint_name, changed_at_ns)`,
		`CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_tokens (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT,
			token_hash BLOB NOT NULL CHECK(length(token_hash) = 32),
			created_at_ns INTEGER NOT NULL,
			expires_at_ns INTEGER,
			revoked_at_ns INTEGER,
			last_used_at_ns INTEGER
		)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate SQLite schema: %w", err)
		}
	}
	if version == 1 {
		for _, statement := range []string{
			"ALTER TABLE endpoints ADD COLUMN flapping_active INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE endpoints ADD COLUMN last_flap_alert_sent_ns INTEGER",
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate SQLite schema to version 2: %w", err)
			}
		}
	}
	if version >= 1 && version < 4 {
		for _, statement := range []string{
			"ALTER TABLE endpoints ADD COLUMN ping_baseline_ns INTEGER",
			"ALTER TABLE endpoints ADD COLUMN last_ping_success_ns INTEGER",
			"ALTER TABLE endpoints ADD COLUMN last_ping_start_ns INTEGER",
			"ALTER TABLE endpoints ADD COLUMN last_ping_fail_ns INTEGER",
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate SQLite schema to version 4: %w", err)
			}
		}
	}
	now := strconv.FormatInt(s.startedAt.UnixNano(), 10)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO metadata(key, value) VALUES
		('digest_since_ns', ?), ('digest_last_check_id', '0')`, now); err != nil {
		return fmt.Errorf("initialize SQLite metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("record SQLite schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite migration: %w", err)
	}
	return nil
}

func (s *StatusStore) syncEndpoints(ctx context.Context, endpoints []config.EndpointConfig) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin endpoint synchronization: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE endpoints SET enabled = 0"); err != nil {
		return fmt.Errorf("disable removed endpoints: %w", err)
	}
	for _, endpoint := range endpoints {
		if _, err := tx.ExecContext(ctx, `INSERT INTO endpoints(name, url, enabled)
			VALUES(?, ?, 1)
			ON CONFLICT(name) DO UPDATE SET
				enabled = 1,
				known = CASE WHEN endpoints.url = excluded.url THEN endpoints.known ELSE 0 END,
				healthy = CASE WHEN endpoints.url = excluded.url THEN endpoints.healthy ELSE 0 END,
				last_checked_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_checked_ns ELSE NULL END,
				last_status_code = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_status_code ELSE 0 END,
				last_latency_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_latency_ns ELSE 0 END,
				consecutive_failures = CASE WHEN endpoints.url = excluded.url THEN endpoints.consecutive_failures ELSE 0 END,
				last_error = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_error ELSE '' END,
				outage_started_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.outage_started_ns ELSE NULL END,
				alert_active = CASE WHEN endpoints.url = excluded.url THEN endpoints.alert_active ELSE 0 END,
				last_alert_sent_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_alert_sent_ns ELSE NULL END,
				last_recovered_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_recovered_ns ELSE NULL END,
				flapping_active = CASE WHEN endpoints.url = excluded.url THEN endpoints.flapping_active ELSE 0 END,
				last_flap_alert_sent_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_flap_alert_sent_ns ELSE NULL END,
				ping_baseline_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.ping_baseline_ns ELSE NULL END,
				last_ping_success_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_ping_success_ns ELSE NULL END,
				last_ping_start_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_ping_start_ns ELSE NULL END,
				last_ping_fail_ns = CASE WHEN endpoints.url = excluded.url THEN endpoints.last_ping_fail_ns ELSE NULL END,
				url = excluded.url`, endpoint.Name, endpoint.URL); err != nil {
			return fmt.Errorf("synchronize endpoint %q: %w", endpoint.Name, err)
		}
		if endpoint.Type == config.EndpointTypePing {
			// Pings are expected from the moment an endpoint is first monitored,
			// so a job that never reports is still detected.
			if _, err := tx.ExecContext(ctx, `UPDATE endpoints SET ping_baseline_ns = COALESCE(ping_baseline_ns, ?)
				WHERE name = ?`, s.startedAt.UnixNano(), endpoint.Name); err != nil {
				return fmt.Errorf("initialize ping baseline for %q: %w", endpoint.Name, err)
			}
		}
		if !endpoint.Flapping.Enabled {
			if _, err := tx.ExecContext(ctx, "UPDATE endpoints SET flapping_active = 0 WHERE name = ?", endpoint.Name); err != nil {
				return fmt.Errorf("disable flapping state for %q: %w", endpoint.Name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit endpoint synchronization: %w", err)
	}
	return nil
}

func (s *StatusStore) Close() error {
	return s.db.Close()
}

func (s *StatusStore) RecordCheck(ctx context.Context, endpoint config.EndpointConfig, result check.Result, checkedAt time.Time) (endpointState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return endpointState{}, fmt.Errorf("begin check transaction: %w", err)
	}
	defer tx.Rollback()
	var previousKnown, previousHealthy int
	if err := tx.QueryRowContext(ctx, "SELECT known, healthy FROM endpoints WHERE name = ? AND enabled = 1", endpoint.Name).
		Scan(&previousKnown, &previousHealthy); err != nil {
		return endpointState{}, fmt.Errorf("load previous endpoint health: %w", err)
	}

	errorText := ""
	if result.Err != nil {
		errorText = result.Err.Error()
	}
	healthy := boolInt(result.Healthy)
	if _, err := tx.ExecContext(ctx, `INSERT INTO checks(
		endpoint_name, endpoint_url, checked_at_ns, healthy, status_code, latency_ns, error
	) VALUES(?, ?, ?, ?, ?, ?, ?)`, endpoint.Name, endpoint.URL, checkedAt.UnixNano(), healthy,
		result.StatusCode, result.Duration.Nanoseconds(), errorText); err != nil {
		return endpointState{}, fmt.Errorf("save check history: %w", err)
	}

	if result.Healthy {
		_, err = tx.ExecContext(ctx, `UPDATE endpoints SET
			known = 1, healthy = 1, last_checked_ns = ?, last_status_code = ?,
			last_latency_ns = ?, consecutive_failures = 0, last_error = '', outage_started_ns = NULL
			WHERE name = ? AND enabled = 1`, checkedAt.UnixNano(), result.StatusCode,
			result.Duration.Nanoseconds(), endpoint.Name)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE endpoints SET
			known = 1, healthy = 0, last_checked_ns = ?, last_status_code = ?,
			last_latency_ns = ?, consecutive_failures = consecutive_failures + 1,
			last_error = ?, outage_started_ns = COALESCE(outage_started_ns, ?)
			WHERE name = ? AND enabled = 1`, checkedAt.UnixNano(), result.StatusCode,
			result.Duration.Nanoseconds(), errorText, checkedAt.UnixNano(), endpoint.Name)
	}
	if err != nil {
		return endpointState{}, fmt.Errorf("update endpoint state: %w", err)
	}
	if previousKnown != 0 && previousHealthy != healthy {
		if _, err := tx.ExecContext(ctx, `INSERT INTO state_transitions(endpoint_name, changed_at_ns, healthy)
			VALUES(?, ?, ?)`, endpoint.Name, checkedAt.UnixNano(), healthy); err != nil {
			return endpointState{}, fmt.Errorf("save endpoint state transition: %w", err)
		}
	}

	state, err := loadEndpointState(ctx, tx, endpoint.Name)
	if err != nil {
		return endpointState{}, err
	}
	if err := tx.Commit(); err != nil {
		return endpointState{}, fmt.Errorf("commit check transaction: %w", err)
	}
	return state, nil
}

func (s *StatusStore) MarkAlertSent(ctx context.Context, endpointName string, sentAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE endpoints SET alert_active = 1, last_alert_sent_ns = ?
		WHERE name = ? AND enabled = 1`, sentAt.UnixNano(), endpointName)
	if err != nil {
		return fmt.Errorf("persist alert state: %w", err)
	}
	return requireUpdated(result, endpointName)
}

func (s *StatusStore) ClearAlert(ctx context.Context, endpointName string, recoveredAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE endpoints SET alert_active = 0, last_recovered_ns = ?
		WHERE name = ? AND enabled = 1`, recoveredAt.UnixNano(), endpointName)
	if err != nil {
		return fmt.Errorf("persist recovery state: %w", err)
	}
	return requireUpdated(result, endpointName)
}

func (s *StatusStore) EndpointState(ctx context.Context, endpointName string) (endpointState, error) {
	return loadEndpointState(ctx, s.db, endpointName)
}

type FlappingEvaluation struct {
	Active       bool
	NeedsAlert   bool
	BecameStable bool
	Transitions  int
}

func (s *StatusStore) EvaluateFlapping(ctx context.Context, endpointName string, at time.Time, window time.Duration, threshold int) (FlappingEvaluation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FlappingEvaluation{}, fmt.Errorf("begin flapping evaluation: %w", err)
	}
	defer tx.Rollback()

	var transitions int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_transitions
		WHERE endpoint_name = ? AND changed_at_ns >= ? AND changed_at_ns <= ?`,
		endpointName, at.Add(-window).UnixNano(), at.UnixNano()).Scan(&transitions); err != nil {
		return FlappingEvaluation{}, fmt.Errorf("count endpoint transitions: %w", err)
	}
	var active int
	var lastAlert sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT flapping_active, last_flap_alert_sent_ns
		FROM endpoints WHERE name = ? AND enabled = 1`, endpointName).Scan(&active, &lastAlert); err != nil {
		return FlappingEvaluation{}, fmt.Errorf("load flapping state: %w", err)
	}

	evaluation := FlappingEvaluation{Transitions: transitions}
	if transitions >= threshold {
		evaluation.Active = true
		if active == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE endpoints SET
				flapping_active = 1, last_flap_alert_sent_ns = NULL WHERE name = ?`, endpointName); err != nil {
				return FlappingEvaluation{}, fmt.Errorf("activate flapping state: %w", err)
			}
		}
		evaluation.NeedsAlert = active == 0 || !lastAlert.Valid
	} else if active != 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE endpoints SET flapping_active = 0 WHERE name = ?", endpointName); err != nil {
			return FlappingEvaluation{}, fmt.Errorf("clear flapping state: %w", err)
		}
		evaluation.BecameStable = true
	}
	if err := tx.Commit(); err != nil {
		return FlappingEvaluation{}, fmt.Errorf("commit flapping evaluation: %w", err)
	}
	return evaluation, nil
}

func (s *StatusStore) MarkFlapAlertSent(ctx context.Context, endpointName string, sentAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE endpoints SET last_flap_alert_sent_ns = ?
		WHERE name = ? AND enabled = 1 AND flapping_active = 1`, sentAt.UnixNano(), endpointName)
	if err != nil {
		return fmt.Errorf("persist flapping alert state: %w", err)
	}
	return requireUpdated(result, endpointName)
}

func requireUpdated(result sql.Result, endpointName string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("endpoint %q is not active", endpointName)
	}
	return nil
}

type scanner interface {
	Scan(...any) error
}

func loadEndpointState(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string) (endpointState, error) {
	var state endpointState
	var alerted int
	var lastSent, outageStarted sql.NullInt64
	err := query.QueryRowContext(ctx, `SELECT consecutive_failures, alert_active,
		last_alert_sent_ns, outage_started_ns FROM endpoints WHERE name = ? AND enabled = 1`, name).
		Scan(&state.failures, &alerted, &lastSent, &outageStarted)
	if err != nil {
		return endpointState{}, fmt.Errorf("load endpoint state %q: %w", name, err)
	}
	state.alerted = alerted != 0
	state.lastSent = nullTimeValue(lastSent)
	state.outageStarted = nullTimeValue(outageStarted)
	return state, nil
}

func (s *StatusStore) Snapshot(ctx context.Context) ([]EndpointStatus, error) {
	rows, err := s.db.QueryContext(ctx, endpointStatusQuery+" WHERE enabled = 1 ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("query endpoint status: %w", err)
	}
	defer rows.Close()
	var statuses []EndpointStatus
	for rows.Next() {
		status, err := scanEndpointStatus(rows)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read endpoint status: %w", err)
	}
	return statuses, nil
}

const endpointStatusQuery = `SELECT name, url, known, healthy, last_checked_ns,
	last_status_code, last_latency_ns, consecutive_failures, last_error,
	outage_started_ns, alert_active, last_alert_sent_ns, last_recovered_ns,
	flapping_active, last_flap_alert_sent_ns, last_ping_success_ns,
	last_ping_start_ns, last_ping_fail_ns FROM endpoints`

func scanEndpointStatus(row scanner) (EndpointStatus, error) {
	var status EndpointStatus
	var known, healthy, alertActive, flappingActive int
	var lastChecked, outageStarted, lastAlertSent, lastRecovered, lastFlapAlert sql.NullInt64
	var lastPing, lastPingStart, lastPingFail sql.NullInt64
	var latencyNS int64
	if err := row.Scan(&status.Name, &status.URL, &known, &healthy, &lastChecked,
		&status.LastStatusCode, &latencyNS, &status.ConsecutiveFailures, &status.LastError,
		&outageStarted, &alertActive, &lastAlertSent, &lastRecovered,
		&flappingActive, &lastFlapAlert, &lastPing, &lastPingStart, &lastPingFail); err != nil {
		return EndpointStatus{}, fmt.Errorf("scan endpoint status: %w", err)
	}
	// The stored URL detects config changes; callers only ever see it redacted.
	status.URL = config.RedactURL(status.URL)
	status.Known = known != 0
	status.Healthy = healthy != 0
	status.AlertActive = alertActive != 0
	status.LastLatencyMillis = time.Duration(latencyNS).Milliseconds()
	if lastChecked.Valid {
		status.LastChecked = time.Unix(0, lastChecked.Int64)
	}
	status.OutageStartedAt = nullTimePointer(outageStarted)
	status.LastAlertSentAt = nullTimePointer(lastAlertSent)
	status.LastRecoveredAt = nullTimePointer(lastRecovered)
	status.Flapping = flappingActive != 0
	status.LastFlapAlertAt = nullTimePointer(lastFlapAlert)
	status.LastPingAt = nullTimePointer(lastPing)
	status.LastPingStartAt = nullTimePointer(lastPingStart)
	status.LastPingFailAt = nullTimePointer(lastPingFail)
	return status, nil
}

func (s *StatusStore) DigestSnapshot(ctx context.Context, at time.Time) (DigestSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DigestSnapshot{}, fmt.Errorf("begin digest snapshot: %w", err)
	}
	defer tx.Rollback()

	var fromNS, lastCheckID int64
	if err := tx.QueryRowContext(ctx, `SELECT
		CAST((SELECT value FROM metadata WHERE key = 'digest_since_ns') AS INTEGER),
		CAST((SELECT value FROM metadata WHERE key = 'digest_last_check_id') AS INTEGER)`).
		Scan(&fromNS, &lastCheckID); err != nil {
		return DigestSnapshot{}, fmt.Errorf("load digest checkpoint: %w", err)
	}
	var snapshotLastID sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT MAX(id) FROM checks").Scan(&snapshotLastID); err != nil {
		return DigestSnapshot{}, fmt.Errorf("snapshot check history: %w", err)
	}
	maxID := lastCheckID
	if snapshotLastID.Valid && snapshotLastID.Int64 > maxID {
		maxID = snapshotLastID.Int64
	}

	rows, err := tx.QueryContext(ctx, endpointStatusQuery+" WHERE enabled = 1 ORDER BY name")
	if err != nil {
		return DigestSnapshot{}, fmt.Errorf("query digest endpoints: %w", err)
	}
	var endpoints []DigestEndpoint
	for rows.Next() {
		status, err := scanEndpointStatus(rows)
		if err != nil {
			rows.Close()
			return DigestSnapshot{}, err
		}
		endpoints = append(endpoints, DigestEndpoint{Status: status})
	}
	if err := rows.Close(); err != nil {
		return DigestSnapshot{}, fmt.Errorf("close digest endpoint query: %w", err)
	}
	for i := range endpoints {
		stats := &endpoints[i].Stats
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN healthy = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN healthy = 0 THEN 1 ELSE 0 END), 0)
			FROM checks WHERE endpoint_name = ? AND id > ? AND id <= ?`,
			endpoints[i].Status.Name, lastCheckID, maxID).
			Scan(&stats.Checks, &stats.PassedChecks, &stats.FailedChecks); err != nil {
			return DigestSnapshot{}, fmt.Errorf("query digest statistics for %q: %w", endpoints[i].Status.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return DigestSnapshot{}, fmt.Errorf("finish digest snapshot: %w", err)
	}
	return DigestSnapshot{
		From:        time.Unix(0, fromNS),
		To:          at,
		StartedAt:   s.startedAt,
		LastCheckID: maxID,
		Endpoints:   endpoints,
	}, nil
}

func (s *StatusStore) CommitDigest(ctx context.Context, snapshot DigestSnapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin digest checkpoint: %w", err)
	}
	defer tx.Rollback()
	for key, value := range map[string]string{
		"digest_since_ns":      strconv.FormatInt(snapshot.To.UnixNano(), 10),
		"digest_last_check_id": strconv.FormatInt(snapshot.LastCheckID, 10),
	} {
		if _, err := tx.ExecContext(ctx, "UPDATE metadata SET value = ? WHERE key = ?", value, key); err != nil {
			return fmt.Errorf("save digest checkpoint: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit digest checkpoint: %w", err)
	}
	return nil
}

func (s *StatusStore) History(ctx context.Context, endpoint string, from, to time.Time) (HistorySummary, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM endpoints WHERE name = ? AND enabled = 1", endpoint).Scan(&exists); err != nil {
		return HistorySummary{}, fmt.Errorf("find endpoint: %w", err)
	}
	if exists == 0 {
		return HistorySummary{}, sql.ErrNoRows
	}
	var checks, passed int
	var average, minimum, maximum sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN healthy = 1 THEN 1 ELSE 0 END), 0),
		AVG(latency_ns / 1000000.0), MIN(latency_ns / 1000000.0), MAX(latency_ns / 1000000.0)
		FROM checks WHERE endpoint_name = ? AND checked_at_ns >= ? AND checked_at_ns <= ?`,
		endpoint, from.UnixNano(), to.UnixNano()).Scan(&checks, &passed, &average, &minimum, &maximum); err != nil {
		return HistorySummary{}, fmt.Errorf("query endpoint history: %w", err)
	}
	summary := HistorySummary{
		Endpoint:     endpoint,
		From:         from,
		To:           to,
		Checks:       checks,
		PassedChecks: passed,
		FailedChecks: checks - passed,
	}
	if checks > 0 {
		availability := float64(passed) / float64(checks) * 100
		summary.AvailabilityPercent = &availability
		summary.AverageLatencyMS = nullableFloat(average)
		summary.MinimumLatencyMS = nullableFloat(minimum)
		summary.MaximumLatencyMS = nullableFloat(maximum)
	}
	return summary, nil
}

func (s *StatusStore) RunMaintenance(ctx context.Context, logger *slog.Logger) {
	prune := func() {
		cutoff := time.Now().AddDate(0, 0, -s.retentionDays)
		count, err := s.PruneHistory(ctx, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("could not prune check history", "error", err)
			}
			return
		}
		if count > 0 {
			logger.Info("pruned old check history", "rows", count, "cutoff", cutoff)
		}
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

func (s *StatusStore) PruneHistory(ctx context.Context, now time.Time) (int64, error) {
	cutoff := now.AddDate(0, 0, -s.retentionDays)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin history pruning: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "DELETE FROM checks WHERE checked_at_ns < ?", cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("delete expired check history: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count pruned check history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM state_transitions WHERE changed_at_ns < ?", cutoff.UnixNano()); err != nil {
		return 0, fmt.Errorf("delete expired state transitions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit history pruning: %w", err)
	}
	return count, nil
}

func (s *StatusStore) Handler(bootstrapToken string, loggers ...*slog.Logger) http.Handler {
	var authLogger *slog.Logger
	if len(loggers) > 0 {
		authLogger = loggers[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, request *http.Request) {
		if err := s.db.PingContext(request.Context()); err != nil {
			http.Error(writer, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte("ok\n"))
	})
	mux.Handle("GET /status", s.requireAPIToken(bootstrapToken, authLogger, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		statuses, err := s.Snapshot(request.Context())
		if err != nil {
			http.Error(writer, "could not load status", http.StatusInternalServerError)
			return
		}
		writeJSON(writer, struct {
			Endpoints []EndpointStatus `json:"endpoints"`
		}{Endpoints: statuses})
	})))
	mux.Handle("GET /history", s.requireAPIToken(bootstrapToken, authLogger, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		endpoint := request.URL.Query().Get("endpoint")
		if endpoint == "" {
			http.Error(writer, "endpoint query parameter is required", http.StatusBadRequest)
			return
		}
		periodText := request.URL.Query().Get("period")
		if periodText == "" {
			periodText = "24h"
		}
		period, err := time.ParseDuration(periodText)
		if err != nil || period <= 0 {
			http.Error(writer, "period must be a positive duration such as 24h or 168h", http.StatusBadRequest)
			return
		}
		to := time.Now()
		summary, err := s.History(request.Context(), endpoint, to.Add(-period), to)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(writer, "endpoint not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(writer, "could not load history", http.StatusInternalServerError)
			return
		}
		writeJSON(writer, summary)
	})))
	return mux
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullTimeValue(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return time.Unix(0, value.Int64)
}

func nullTimePointer(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	parsed := time.Unix(0, value.Int64)
	return &parsed
}

func nullableFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}
