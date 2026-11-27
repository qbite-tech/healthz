package monitor

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qbite-tech/healthz/internal/check"
	"github.com/qbite-tech/healthz/internal/config"
)

func TestSQLiteStateSurvivesRestartAndDeduplicatesAlert(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "healthz.db")
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	cfg := config.Config{
		Alerts:    config.AlertConfig{FailureThreshold: 1, SendRecovery: true},
		Endpoints: []config.EndpointConfig{endpoint},
	}
	firstFailure := time.Now().Add(-2 * time.Minute).Round(0)
	failed := check.Result{Duration: 400 * time.Millisecond, Err: context.DeadlineExceeded}

	store := openTestStore(t, path, endpoint)
	firstSender := &recordingSender{}
	runner := NewRunner(cfg, nil, firstSender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}
	runner.processResult(ctx, endpoint, &state, failed, firstFailure)
	if len(firstSender.messages) != 1 || !state.alerted {
		t.Fatalf("initial alert messages = %d, state = %+v", len(firstSender.messages), state)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, endpoint)
	secondSender := &recordingSender{}
	runner = NewRunner(cfg, nil, secondSender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state = endpointState{}
	runner.processResult(ctx, endpoint, &state, failed, firstFailure.Add(time.Minute))
	if len(secondSender.messages) != 0 {
		t.Fatalf("sent %d duplicate alerts after restart, want 0", len(secondSender.messages))
	}
	if state.failures != 2 || !state.alerted || !state.outageStarted.Equal(firstFailure) {
		t.Fatalf("restored state = %+v, want two failures and outage at %s", state, firstFailure)
	}

	statuses, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].OutageStartedAt == nil || !statuses[0].OutageStartedAt.Equal(firstFailure) {
		t.Fatalf("persisted status = %+v", statuses)
	}
	if !statuses[0].AlertActive || statuses[0].LastAlertSentAt == nil {
		t.Fatalf("persisted alert state = %+v", statuses[0])
	}
	digest := mustDigestSnapshot(t, store, time.Now())
	if digest.Endpoints[0].Stats.Checks != 2 || digest.Endpoints[0].Stats.FailedChecks != 2 {
		t.Fatalf("digest statistics after restart = %+v", digest.Endpoints[0].Stats)
	}

	summary, err := store.History(ctx, endpoint.Name, firstFailure.Add(-time.Second), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Checks != 2 || summary.PassedChecks != 0 || summary.AverageLatencyMS == nil || *summary.AverageLatencyMS != 400 {
		t.Fatalf("history summary = %+v", summary)
	}

	healthy := check.Result{Healthy: true, StatusCode: 200, Duration: 20 * time.Millisecond}
	runner.processResult(ctx, endpoint, &state, healthy, time.Now())
	if len(secondSender.messages) != 1 {
		t.Fatalf("recovery messages = %d, want 1", len(secondSender.messages))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, endpoint)
	t.Cleanup(func() { _ = store.Close() })
	statuses, err = store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].AlertActive || statuses[0].OutageStartedAt != nil || statuses[0].ConsecutiveFailures != 0 || !statuses[0].Healthy {
		t.Fatalf("recovered state after restart = %+v", statuses[0])
	}
	if statuses[0].LastAlertSentAt == nil || statuses[0].LastRecoveredAt == nil {
		t.Fatalf("last alert and recovery were not retained: %+v", statuses[0])
	}
}

func TestDigestCheckpointSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "healthz.db")
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	store := openTestStore(t, path, endpoint)
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true, Duration: 10 * time.Millisecond}, time.Now())
	snapshot := mustDigestSnapshot(t, store, time.Now())
	if snapshot.Endpoints[0].Stats.Checks != 1 {
		t.Fatalf("initial digest stats = %+v", snapshot.Endpoints[0].Stats)
	}
	if err := store.CommitDigest(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, endpoint)
	t.Cleanup(func() { _ = store.Close() })
	snapshot = mustDigestSnapshot(t, store, time.Now())
	if snapshot.Endpoints[0].Stats.Checks != 0 {
		t.Fatalf("digest checkpoint was not restored: %+v", snapshot.Endpoints[0].Stats)
	}
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: false}, time.Now())
	snapshot = mustDigestSnapshot(t, store, time.Now())
	if snapshot.Endpoints[0].Stats.Checks != 1 || snapshot.Endpoints[0].Stats.FailedChecks != 1 {
		t.Fatalf("new digest stats after restart = %+v", snapshot.Endpoints[0].Stats)
	}
}

func TestHistoryAvailabilityLatencyAndRetention(t *testing.T) {
	ctx := context.Background()
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	store, err := OpenStatusStore(filepath.Join(t.TempDir(), "healthz.db"), []config.EndpointConfig{endpoint}, 7)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true, Duration: 100 * time.Millisecond}, now.Add(-time.Hour))
	mustRecordCheck(t, store, endpoint, check.Result{Duration: 300 * time.Millisecond, Err: context.DeadlineExceeded}, now)
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true, Duration: 5 * time.Millisecond}, now.AddDate(0, 0, -8))

	summary, err := store.History(ctx, endpoint.Name, now.Add(-2*time.Hour), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Checks != 2 || summary.PassedChecks != 1 || summary.FailedChecks != 1 {
		t.Fatalf("history counts = %+v", summary)
	}
	if summary.AvailabilityPercent == nil || math.Abs(*summary.AvailabilityPercent-50) > 0.001 {
		t.Fatalf("availability = %+v", summary.AvailabilityPercent)
	}
	if summary.AverageLatencyMS == nil || math.Abs(*summary.AverageLatencyMS-200) > 0.001 {
		t.Fatalf("average latency = %+v", summary.AverageLatencyMS)
	}

	pruned, err := store.PruneHistory(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned rows = %d, want 1", pruned)
	}
	all, err := store.History(ctx, endpoint.Name, now.AddDate(-1, 0, 0), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if all.Checks != 2 {
		t.Fatalf("checks after retention pruning = %d, want 2", all.Checks)
	}
}

func TestHistoryHandler(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "Main website", URL: "https://example.com"}
	store := NewStatusStore([]config.EndpointConfig{endpoint})
	t.Cleanup(func() { _ = store.Close() })
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true, Duration: 25 * time.Millisecond}, time.Now())

	request := httptest.NewRequest(http.MethodGet, "/history?endpoint=Main%20website&period=24h", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	store.Handler("test-token").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("history status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, expected := range []string{`"checks":1`, `"passed_checks":1`, `"average_latency_ms":25`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("history response does not contain %s: %s", expected, response.Body.String())
		}
	}
}

func TestStatusAndHistoryRequireAuthentication(t *testing.T) {
	store := NewStatusStore([]config.EndpointConfig{{Name: "site", URL: "https://example.com"}})
	t.Cleanup(func() { _ = store.Close() })
	handler := store.Handler("correct-token")

	for _, path := range []string{"/status", "/history?endpoint=site"} {
		for name, header := range map[string]string{
			"missing":      "",
			"wrong token":  "Bearer wrong-token",
			"wrong scheme": "Basic correct-token",
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.Header.Set("Authorization", header)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", response.Code)
				}
				if got := response.Header().Get("WWW-Authenticate"); got != `Bearer realm="healthz"` {
					t.Fatalf("WWW-Authenticate = %q", got)
				}
			})
		}
	}
}

func TestHealthzDoesNotRequireAuthentication(t *testing.T) {
	store := NewStatusStore([]config.EndpointConfig{{Name: "site", URL: "https://example.com"}})
	t.Cleanup(func() { _ = store.Close() })
	response := httptest.NewRecorder()
	store.Handler("test-token").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}

func TestFlappingStateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "healthz.db")
	endpoint := config.EndpointConfig{
		Name: "site",
		URL:  "https://example.com",
		Flapping: config.FlappingConfig{
			Enabled:     true,
			Transitions: 3,
			Window:      config.Duration{Duration: 10 * time.Minute},
		},
	}
	store := openTestStore(t, path, endpoint)
	base := time.Now()
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true}, base)
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: false}, base.Add(time.Minute))
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true}, base.Add(2*time.Minute))
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: false}, base.Add(3*time.Minute))
	evaluation, err := store.EvaluateFlapping(ctx, endpoint.Name, base.Add(3*time.Minute), 10*time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !evaluation.Active || !evaluation.NeedsAlert {
		t.Fatalf("initial flapping evaluation = %+v", evaluation)
	}
	evaluation, err = store.EvaluateFlapping(ctx, endpoint.Name, base.Add(3*time.Minute), 10*time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !evaluation.NeedsAlert {
		t.Fatalf("unsent flapping alert was not retried: %+v", evaluation)
	}
	if err := store.MarkFlapAlertSent(ctx, endpoint.Name, base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, endpoint)
	t.Cleanup(func() { _ = store.Close() })
	evaluation, err = store.EvaluateFlapping(ctx, endpoint.Name, base.Add(4*time.Minute), 10*time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !evaluation.Active || evaluation.NeedsAlert {
		t.Fatalf("restored flapping evaluation = %+v, want active without duplicate alert", evaluation)
	}
}

func TestMigratesVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthz.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE endpoints (
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
		last_recovered_ns INTEGER
	);
	PRAGMA user_version = 1;`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	store := openTestStore(t, path, endpoint)
	t.Cleanup(func() { _ = store.Close() })
	statuses, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Name != endpoint.Name || statuses[0].Flapping {
		t.Fatalf("migrated status = %+v", statuses)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	var tokenTable string
	if err := store.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'api_tokens'").Scan(&tokenTable); err != nil {
		t.Fatalf("API token table was not migrated: %v", err)
	}
}

func TestCurrentSchemaMigrationIsReadOnly(t *testing.T) {
	store := NewStatusStore([]config.EndpointConfig{{Name: "site", URL: "https://example.com"}})
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.Exec("PRAGMA query_only = ON"); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateSchema(context.Background()); err != nil {
		t.Fatalf("current schema migration attempted a write: %v", err)
	}
}

func openTestStore(t *testing.T, path string, endpoint config.EndpointConfig) *StatusStore {
	t.Helper()
	store, err := OpenStatusStore(path, []config.EndpointConfig{endpoint}, 30)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
