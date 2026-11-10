package monitor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runeQ/healthz/internal/check"
	"github.com/runeQ/healthz/internal/config"
)

const testPingKey = "0123456789abcdef0123456789abcdef"

func pingEndpoint() config.EndpointConfig {
	return config.EndpointConfig{
		Name: "backup", Type: config.EndpointTypePing, URL: config.PingTarget("BACKUP_PING_KEY"),
		PingKeyEnv: "BACKUP_PING_KEY", Schedule: "30 2 * * *", Timezone: "UTC",
		Grace: config.Duration{Duration: 3 * time.Hour}, FailureThreshold: 1,
	}
}

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestNewPingKeysValidatesKeys(t *testing.T) {
	endpoint := pingEndpoint()
	other := pingEndpoint()
	other.Name, other.PingKeyEnv = "sync", "SYNC_PING_KEY"
	http := config.EndpointConfig{Name: "site", Type: config.EndpointTypeHTTP}

	keys, err := NewPingKeys([]config.EndpointConfig{http, endpoint}, lookupFrom(map[string]string{"BACKUP_PING_KEY": testPingKey}))
	if err != nil || len(keys) != 1 {
		t.Fatalf("NewPingKeys() = %v, %v", keys, err)
	}
	for name, test := range map[string]struct {
		endpoints []config.EndpointConfig
		env       map[string]string
	}{
		"missing":   {[]config.EndpointConfig{endpoint}, map[string]string{}},
		"empty":     {[]config.EndpointConfig{endpoint}, map[string]string{"BACKUP_PING_KEY": ""}},
		"too short": {[]config.EndpointConfig{endpoint}, map[string]string{"BACKUP_PING_KEY": "abc123"}},
		"slash":     {[]config.EndpointConfig{endpoint}, map[string]string{"BACKUP_PING_KEY": testPingKey + "/fail"}},
		"shared": {[]config.EndpointConfig{endpoint, other},
			map[string]string{"BACKUP_PING_KEY": testPingKey, "SYNC_PING_KEY": testPingKey}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewPingKeys(test.endpoints, lookupFrom(test.env))
			if err == nil {
				t.Fatal("NewPingKeys() succeeded, want error")
			}
			if strings.Contains(err.Error(), testPingKey) {
				t.Fatalf("error reveals the key: %v", err)
			}
		})
	}
}

func TestPingHandlerRecordsPings(t *testing.T) {
	endpoint := pingEndpoint()
	store := NewStatusStore([]config.EndpointConfig{endpoint})
	t.Cleanup(func() { _ = store.Close() })
	keys, err := NewPingKeys([]config.EndpointConfig{endpoint}, lookupFrom(map[string]string{"BACKUP_PING_KEY": testPingKey}))
	if err != nil {
		t.Fatal(err)
	}
	var notified []string
	handler := store.PingHandler(keys, func(name string) { notified = append(notified, name) }, nil)

	serve := func(method, path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
		return response
	}
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/ping/" + testPingKey + "/start", http.StatusOK},
		{http.MethodPost, "/ping/" + testPingKey, http.StatusOK},
		{http.MethodHead, "/ping/" + testPingKey + "/0", http.StatusOK},
		{http.MethodGet, "/ping/" + testPingKey + "/fail", http.StatusOK},
		{http.MethodGet, "/ping/" + testPingKey + "/2", http.StatusOK},
		{http.MethodGet, "/ping/" + testPingKey + "/256", http.StatusNotFound},
		{http.MethodGet, "/ping/" + testPingKey + "/01", http.StatusNotFound},
		{http.MethodGet, "/ping/" + testPingKey + "/log", http.StatusNotFound},
		{http.MethodGet, "/ping/wrong-key", http.StatusNotFound},
		{http.MethodGet, "/ping/wrong-key/fail", http.StatusNotFound},
		{http.MethodDelete, "/ping/" + testPingKey, http.StatusMethodNotAllowed},
	} {
		if response := serve(test.method, test.path); response.Code != test.status {
			t.Errorf("%s %s = %d, want %d", test.method, test.path, response.Code, test.status)
		}
	}
	if len(notified) != 5 || notified[0] != endpoint.Name {
		t.Fatalf("notified = %v, want five wakes for %q", notified, endpoint.Name)
	}

	state, err := store.PingState(context.Background(), endpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline.IsZero() || state.LastStart.IsZero() || state.LastSuccess.IsZero() || state.LastFail.IsZero() {
		t.Fatalf("ping state = %+v, want all events recorded", state)
	}
	if !state.LastFail.After(state.LastSuccess) {
		t.Fatalf("exit status 2 was not recorded as the latest failure: %+v", state)
	}
	statuses, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].LastPingAt == nil || statuses[0].LastPingStartAt == nil || statuses[0].LastPingFailAt == nil {
		t.Fatalf("status = %+v, want ping times", statuses[0])
	}
}

func TestLogLimiterSuppressesWithinInterval(t *testing.T) {
	limiter := &logLimiter{interval: time.Minute}
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		at         time.Duration
		log        bool
		suppressed int
	}{
		{0, true, 0},
		{time.Second, false, 0},
		{59 * time.Second, false, 0},
		{time.Minute, true, 2},
		{time.Minute + time.Second, false, 0},
		{3 * time.Minute, true, 1},
		{5 * time.Minute, true, 0},
	} {
		log, suppressed := limiter.record(start.Add(step.at))
		if log != step.log || suppressed != step.suppressed {
			t.Errorf("record(+%s) = %v, %d; want %v, %d", step.at, log, suppressed, step.log, step.suppressed)
		}
	}
}

func TestPingHandlerRateLimitsUnknownKeyLogs(t *testing.T) {
	endpoint := pingEndpoint()
	store := NewStatusStore([]config.EndpointConfig{endpoint})
	t.Cleanup(func() { _ = store.Close() })
	keys, err := NewPingKeys([]config.EndpointConfig{endpoint}, lookupFrom(map[string]string{"BACKUP_PING_KEY": testPingKey}))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	handler := store.PingHandler(keys, nil, slog.New(slog.NewTextHandler(&logs, nil)))

	const guessedKey = "fedcba9876543210fedcba9876543210"
	for range 100 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ping/"+guessedKey, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("unknown key status = %d, want 404", response.Code)
		}
	}
	if count := strings.Count(logs.String(), "rejected ping with unknown key"); count != 1 {
		t.Fatalf("logged %d rejections for a burst of 100, want 1:\n%s", count, logs.String())
	}
	if strings.Contains(logs.String(), guessedKey) {
		t.Fatalf("log reveals the rejected key:\n%s", logs.String())
	}
}

func TestPingEndpointAlertsOnFailureAndRecovers(t *testing.T) {
	endpoint := pingEndpoint()
	cfg := config.Config{
		Alerts:    config.AlertConfig{FailureThreshold: 2, SendRecovery: true},
		Endpoints: []config.EndpointConfig{endpoint},
	}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	sender := &recordingSender{}
	checker := check.NewPingChecker(store)
	runner := NewRunner(cfg, checker, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	state := endpointState{}

	now := time.Now()
	runner.processResult(ctx, endpoint, &state, checker.Check(ctx, endpoint), now)
	if len(sender.messages) != 0 {
		t.Fatalf("new ping endpoint alerted before its first run was due: %+v", sender.messages)
	}
	if err := store.RecordPing(ctx, endpoint.Name, PingFail, now); err != nil {
		t.Fatal(err)
	}
	runner.processResult(ctx, endpoint, &state, checker.Check(ctx, endpoint), now.Add(time.Second))
	if len(sender.messages) != 1 || !strings.HasPrefix(sender.messages[0].Subject, "[DOWN] backup") ||
		!strings.Contains(sender.messages[0].Body, "job reported failure") {
		t.Fatalf("messages after failure ping = %+v", sender.messages)
	}
	if err := store.RecordPing(ctx, endpoint.Name, PingSuccess, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runner.processResult(ctx, endpoint, &state, checker.Check(ctx, endpoint), now.Add(time.Minute))
	if len(sender.messages) != 2 || !strings.HasPrefix(sender.messages[1].Subject, "[RECOVERED] backup") {
		t.Fatalf("messages after success ping = %+v", sender.messages)
	}
}

func TestRunnerWakeRunsPingCheckEarly(t *testing.T) {
	endpoint := pingEndpoint()
	runner := NewRunner(config.Config{Endpoints: []config.EndpointConfig{endpoint}}, nil, nil, nil, nil)
	runner.Wake(endpoint.Name)
	runner.Wake(endpoint.Name) // coalesced, must not block
	runner.Wake("unknown")
	start := time.Now()
	if !runner.waitForNextCheck(context.Background(), endpoint, time.Hour) {
		t.Fatal("wait was cancelled")
	}
	if time.Since(start) > time.Second {
		t.Fatal("wake did not end the wait early")
	}
}

func TestMigratesVersionThreeDatabaseForPings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthz.db")
	endpoint := pingEndpoint()
	store := openTestStore(t, path, endpoint)
	for _, statement := range []string{
		"ALTER TABLE endpoints DROP COLUMN ping_baseline_ns",
		"ALTER TABLE endpoints DROP COLUMN last_ping_success_ns",
		"ALTER TABLE endpoints DROP COLUMN last_ping_start_ns",
		"ALTER TABLE endpoints DROP COLUMN last_ping_fail_ns",
		"PRAGMA user_version = 3",
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, endpoint)
	t.Cleanup(func() { _ = store.Close() })
	state, err := store.PingState(context.Background(), endpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline.IsZero() {
		t.Fatal("migrated ping endpoint has no baseline")
	}
	if err := store.RecordPing(context.Background(), endpoint.Name, PingSuccess, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestPingStateResetsWhenKeyVariableChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthz.db")
	endpoint := pingEndpoint()
	store := openTestStore(t, path, endpoint)
	if err := store.RecordPing(context.Background(), endpoint.Name, PingFail, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	endpoint.PingKeyEnv = "NEW_BACKUP_PING_KEY"
	endpoint.URL = config.PingTarget(endpoint.PingKeyEnv)
	store = openTestStore(t, path, endpoint)
	t.Cleanup(func() { _ = store.Close() })
	state, err := store.PingState(context.Background(), endpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !state.LastFail.IsZero() || state.Baseline.IsZero() {
		t.Fatalf("state after key change = %+v, want reset with a new baseline", state)
	}
}
