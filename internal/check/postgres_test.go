package check

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/runeQ/healthz/internal/config"
)

type fakeRows struct {
	rows [][]any
	next int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.next >= len(r.rows) {
		return false
	}
	r.next++
	return true
}

func (r *fakeRows) Values() ([]any, error) { return r.rows[r.next-1], nil }
func (r *fakeRows) Err() error             { return r.err }
func (r *fakeRows) Close()                 {}

func TestReadHealthRow(t *testing.T) {
	for name, test := range map[string]struct {
		rows        [][]any
		healthy     bool
		detail      string
		errContains string
	}{
		"healthy":                {rows: [][]any{{true}}, healthy: true},
		"unhealthy with detail":  {rows: [][]any{{false, " worker:office stale 7m "}}, detail: "worker:office stale 7m"},
		"null healthy":           {rows: [][]any{{nil}}, detail: "query returned NULL for healthy"},
		"null detail":            {rows: [][]any{{true, nil}}, healthy: true},
		"no rows":                {errContains: "no rows"},
		"two rows":               {rows: [][]any{{true}, {true}}, errContains: "more than one row"},
		"three columns":          {rows: [][]any{{true, "a", "b"}}, errContains: "3 columns"},
		"non-boolean first":      {rows: [][]any{{int64(1)}}, errContains: "expected boolean"},
		"non-text second column": {rows: [][]any{{false, int64(7)}}, errContains: "expected text"},
	} {
		t.Run(name, func(t *testing.T) {
			healthy, detail, err := readHealthRow(&fakeRows{rows: test.rows})
			if test.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), test.errContains) {
					t.Fatalf("error = %v, want containing %q", err, test.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("readHealthRow() error = %v", err)
			}
			if healthy != test.healthy || detail != test.detail {
				t.Fatalf("readHealthRow() = %v, %q; want %v, %q", healthy, detail, test.healthy, test.detail)
			}
		})
	}
}

func TestReadHealthRowReportsRowsError(t *testing.T) {
	_, _, err := readHealthRow(&fakeRows{err: errors.New("canceling statement due to statement timeout")})
	if err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("error = %v, want statement timeout", err)
	}
}

func TestTruncateKeepsWholeRunes(t *testing.T) {
	if got := truncate("æøå", 3); got != "æ…" {
		t.Fatalf("truncate() = %q, want %q", got, "æ…")
	}
}

func TestPostgresCheckerRequiresDSN(t *testing.T) {
	checker := &PostgresChecker{lookupEnv: func(string) (string, bool) { return "", false }}
	result := checker.Check(context.Background(), postgresEndpoint("SELECT true"))
	if result.Healthy || result.Err == nil || !strings.Contains(result.Err.Error(), "TEST_DSN is not set") {
		t.Fatalf("result = %+v, want missing DSN error", result)
	}
}

func TestPostgresCheckerDoesNotRevealInvalidDSN(t *testing.T) {
	const dsn = "postgres://user:s3cret@host:notaport/db"
	checker := &PostgresChecker{lookupEnv: func(string) (string, bool) { return dsn, true }}
	result := checker.Check(context.Background(), postgresEndpoint("SELECT true"))
	if result.Healthy || result.Err == nil || strings.Contains(result.Err.Error(), "s3cret") {
		t.Fatalf("result = %+v, want an error without the DSN", result)
	}
}

func TestPostgresCheckerAgainstDatabase(t *testing.T) {
	dsn := startPostgres(t)
	checker := &PostgresChecker{lookupEnv: func(string) (string, bool) { return dsn, true }}
	for name, test := range map[string]struct {
		query       string
		timeout     time.Duration
		healthy     bool
		errContains string
	}{
		"healthy":            {query: "SELECT now() > now() - interval '1 minute'", healthy: true},
		"unhealthy detail":   {query: "SELECT false, 'worker:office stale 7m'", errContains: "worker:office stale 7m"},
		"writes are refused": {query: "CREATE TABLE healthz_probe(id int)", errContains: "read-only transaction"},
		"one statement only": {query: "SELECT true; SELECT true", errContains: "multiple commands"},
		"statement timeout":  {query: "SELECT pg_sleep(5) IS NOT NULL", timeout: 500 * time.Millisecond, errContains: "timeout"},
		"query error":        {query: "SELECT missing_column", errContains: "missing_column"},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint := postgresEndpoint(test.query)
			if test.timeout > 0 {
				endpoint.Timeout.Duration = test.timeout
			}
			result := checker.Check(context.Background(), endpoint)
			if result.Healthy != test.healthy {
				t.Fatalf("healthy = %v, want %v (error %v)", result.Healthy, test.healthy, result.Err)
			}
			if test.errContains != "" && (result.Err == nil || !strings.Contains(result.Err.Error(), test.errContains)) {
				t.Fatalf("error = %v, want containing %q", result.Err, test.errContains)
			}
			if result.Duration <= 0 {
				t.Fatalf("duration = %s, want positive", result.Duration)
			}
		})
	}
}

func postgresEndpoint(query string) config.EndpointConfig {
	return config.EndpointConfig{
		Name:    "database",
		Type:    config.EndpointTypePostgres,
		DSNEnv:  "TEST_DSN",
		Query:   query,
		Timeout: config.Duration{Duration: 10 * time.Second},
	}
}

// startPostgres runs a disposable local cluster, or skips without PostgreSQL binaries.
func startPostgres(t *testing.T) string {
	t.Helper()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		installed, _ := filepath.Glob("/usr/lib/postgresql/*/bin/initdb")
		if len(installed) == 0 {
			t.Skip("PostgreSQL initdb is required for the database integration test")
		}
		sort.Strings(installed)
		initdb = installed[len(installed)-1]
	}
	pgCtl := filepath.Join(filepath.Dir(initdb), "pg_ctl")
	// Unix socket paths are limited to about 100 bytes, so avoid t.TempDir's long names.
	dir, err := os.MkdirTemp("", "healthz-pg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	data := filepath.Join(dir, "data")
	run := func(name string, args ...string) {
		t.Helper()
		if output, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", filepath.Base(name), err, output)
		}
	}
	run(initdb, "-D", data, "-U", "postgres", "-A", "trust", "--no-sync")
	run(pgCtl, "-D", data, "-l", filepath.Join(dir, "log"), "-w", "-o", "-c listen_addresses='' -k "+dir, "start")
	t.Cleanup(func() { _ = exec.Command(pgCtl, "-D", data, "-m", "immediate", "-w", "stop").Run() })
	return "host=" + dir + " user=postgres dbname=postgres"
}
