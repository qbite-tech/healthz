package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/qbite-tech/healthz/internal/alert"
	"github.com/qbite-tech/healthz/internal/check"
	"github.com/qbite-tech/healthz/internal/config"
)

type recordingSender struct {
	messages []alert.Message
	err      error
}

type sequenceChecker struct {
	results []check.Result
	calls   int
}

func (c *sequenceChecker) Check(_ context.Context, _ config.EndpointConfig) check.Result {
	result := c.results[min(c.calls, len(c.results)-1)]
	c.calls++
	return result
}

func (s *recordingSender) Send(_ context.Context, message alert.Message) error {
	if s.err != nil {
		return s.err
	}
	s.messages = append(s.messages, message)
	return nil
}

func TestRunnerSendsDownOnceAndRecovery(t *testing.T) {
	endpointRecipients := []string{"team@example.com", "operations@example.com"}
	endpoint := config.EndpointConfig{
		Name: "site", URL: "https://example.com", AlertTo: endpointRecipients,
		AlertSubjects: config.AlertSubjectConfig{
			Down:      "[CRITICAL] {name} failed {failures} times",
			Recovered: "[RESOLVED] {name} returned {status}",
		},
	}
	cfg := config.Config{
		Alerts:    config.AlertConfig{FailureThreshold: 2, SendRecovery: true},
		Endpoints: []config.EndpointConfig{endpoint},
	}
	sender := &recordingSender{}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	runner := NewRunner(cfg, nil, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}
	failed := check.Result{Err: errors.New("connection refused")}
	now := time.Now()

	runner.processResult(context.Background(), endpoint, &state, failed, now)
	if len(sender.messages) != 0 {
		t.Fatalf("sent %d messages before threshold, want 0", len(sender.messages))
	}
	runner.processResult(context.Background(), endpoint, &state, failed, now.Add(time.Second))
	runner.processResult(context.Background(), endpoint, &state, failed, now.Add(2*time.Second))
	if len(sender.messages) != 1 {
		t.Fatalf("sent %d outage messages, want 1", len(sender.messages))
	}
	if got := sender.messages[0].Recipients; len(got) != 2 || got[0] != endpointRecipients[0] || got[1] != endpointRecipients[1] {
		t.Fatalf("outage recipients = %v, want %v", got, endpointRecipients)
	}
	if got := sender.messages[0].Subject; got != "[CRITICAL] site failed 2 times" {
		t.Fatalf("outage subject = %q", got)
	}

	healthy := check.Result{Healthy: true, StatusCode: 200, Duration: 10 * time.Millisecond}
	runner.processResult(context.Background(), endpoint, &state, healthy, now.Add(3*time.Second))
	if len(sender.messages) != 2 {
		t.Fatalf("sent %d total messages, want outage and recovery", len(sender.messages))
	}
	if got := sender.messages[1].Recipients; len(got) != 2 || got[0] != endpointRecipients[0] || got[1] != endpointRecipients[1] {
		t.Fatalf("recovery recipients = %v, want %v", got, endpointRecipients)
	}
	if got := sender.messages[1].Subject; got != "[RESOLVED] site returned 200" {
		t.Fatalf("recovery subject = %q", got)
	}
	if state.alerted || state.failures != 0 {
		t.Fatalf("state after recovery = %+v, want reset", state)
	}
}

func TestRunnerRetriesEmailAfterSendFailure(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	cfg := config.Config{Alerts: config.AlertConfig{FailureThreshold: 1}, Endpoints: []config.EndpointConfig{endpoint}}
	sender := &recordingSender{err: errors.New("SMTP unavailable")}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	runner := NewRunner(cfg, nil, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}
	failed := check.Result{Err: errors.New("connection refused")}

	runner.processResult(context.Background(), endpoint, &state, failed, time.Now())
	if state.alerted {
		t.Fatal("failed email marked endpoint as alerted")
	}
	sender.err = nil
	runner.processResult(context.Background(), endpoint, &state, failed, time.Now().Add(time.Second))
	if !state.alerted || len(sender.messages) != 1 {
		t.Fatalf("retry state = %+v, messages = %d", state, len(sender.messages))
	}
}

func TestRunnerConfirmsFailureBeforeAlerting(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com", RecheckBeforeAlert: true}
	cfg := config.Config{Alerts: config.AlertConfig{FailureThreshold: 1}, Endpoints: []config.EndpointConfig{endpoint}}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	sender := &recordingSender{}
	checker := &sequenceChecker{results: []check.Result{
		{Err: errors.New("temporary network error")},
		{Healthy: true, StatusCode: 200},
	}}
	runner := NewRunner(cfg, checker, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}

	runner.runCheckCycle(context.Background(), endpoint, &state)
	if checker.calls != 2 {
		t.Fatalf("check calls = %d, want initial check and immediate confirmation", checker.calls)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("sent %d messages after healthy confirmation, want 0", len(sender.messages))
	}
	if state.failures != 0 {
		t.Fatalf("failure count after healthy confirmation = %d, want 0", state.failures)
	}
	snapshot := mustDigestSnapshot(t, store, time.Now())
	if snapshot.Endpoints[0].Stats.Checks != 2 || snapshot.Endpoints[0].Stats.PassedChecks != 1 || snapshot.Endpoints[0].Stats.FailedChecks != 1 {
		t.Fatalf("confirmation history = %+v", snapshot.Endpoints[0].Stats)
	}
}

func TestRunnerAlertsAfterConfirmedFailure(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com", RecheckBeforeAlert: true}
	cfg := config.Config{Alerts: config.AlertConfig{FailureThreshold: 1}, Endpoints: []config.EndpointConfig{endpoint}}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	sender := &recordingSender{}
	checker := &sequenceChecker{results: []check.Result{
		{Err: errors.New("connection refused")},
		{Err: errors.New("connection refused")},
	}}
	runner := NewRunner(cfg, checker, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}

	runner.runCheckCycle(context.Background(), endpoint, &state)
	if checker.calls != 2 || len(sender.messages) != 1 {
		t.Fatalf("check calls = %d, messages = %d; want two checks and one alert", checker.calls, len(sender.messages))
	}
	if state.failures != 2 || !state.alerted {
		t.Fatalf("confirmed outage state = %+v", state)
	}
}

func TestRunnerAddsBoundedScheduleJitter(t *testing.T) {
	endpoint := config.EndpointConfig{
		Frequency:      config.Duration{Duration: time.Minute},
		ScheduleJitter: config.Duration{Duration: 10 * time.Second},
	}
	runner := &Runner{jitter: func(maximum time.Duration) time.Duration {
		if maximum != 10*time.Second {
			t.Fatalf("jitter maximum = %s, want 10s", maximum)
		}
		return 4 * time.Second
	}}
	if got := runner.nextCheckDelay(endpoint); got != 64*time.Second {
		t.Fatalf("next check delay = %s, want 1m4s", got)
	}
	for range 100 {
		got := randomJitter(10 * time.Millisecond)
		if got < 0 || got >= 10*time.Millisecond {
			t.Fatalf("random jitter %s is outside [0, 10ms)", got)
		}
	}
}

func TestRunnerDetectsFlappingOnceAndClearsWhenStable(t *testing.T) {
	recipients := []string{"team@example.com", "operations@example.com"}
	endpoint := config.EndpointConfig{
		Name:    "site",
		URL:     "https://example.com",
		AlertTo: recipients,
		Flapping: config.FlappingConfig{
			Enabled:     true,
			Transitions: 3,
			Window:      config.Duration{Duration: 10 * time.Minute},
		},
		AlertSubjects: config.AlertSubjectConfig{Flapping: "[UNSTABLE] {name}: {transitions} changes in {window}"},
	}
	cfg := config.Config{Alerts: config.AlertConfig{FailureThreshold: 10}, Endpoints: []config.EndpointConfig{endpoint}}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	sender := &recordingSender{}
	runner := NewRunner(cfg, nil, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}
	base := time.Now()

	runner.processResult(context.Background(), endpoint, &state, check.Result{Healthy: true, StatusCode: 200}, base)
	runner.processResult(context.Background(), endpoint, &state, check.Result{Err: errors.New("down")}, base.Add(time.Minute))
	runner.processResult(context.Background(), endpoint, &state, check.Result{Healthy: true, StatusCode: 200}, base.Add(2*time.Minute))
	runner.processResult(context.Background(), endpoint, &state, check.Result{Err: errors.New("down")}, base.Add(3*time.Minute))
	if len(sender.messages) != 1 || sender.messages[0].Subject != "[UNSTABLE] site: 3 changes in 10m0s" {
		t.Fatalf("flapping messages = %+v, want one flapping alert", sender.messages)
	}
	if got := sender.messages[0].Recipients; len(got) != 2 || got[0] != recipients[0] || got[1] != recipients[1] {
		t.Fatalf("flapping recipients = %v, want %v", got, recipients)
	}
	statuses, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !statuses[0].Flapping || statuses[0].LastFlapAlertAt == nil {
		t.Fatalf("flapping status = %+v", statuses[0])
	}

	runner.processResult(context.Background(), endpoint, &state, check.Result{Healthy: true, StatusCode: 200}, base.Add(4*time.Minute))
	runner.processResult(context.Background(), endpoint, &state, check.Result{Healthy: true, StatusCode: 200}, base.Add(20*time.Minute))
	statuses, err = store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Flapping {
		t.Fatalf("flapping state did not clear after stable window: %+v", statuses[0])
	}
	if len(sender.messages) != 1 {
		t.Fatalf("sent %d flapping messages, want one", len(sender.messages))
	}
}

func TestRecoveryEmailOmitsHTTPStatusForDatabaseChecks(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "workers", Type: config.EndpointTypePostgres, URL: config.PostgresTarget("DSN")}
	cfg := config.Config{
		Alerts:    config.AlertConfig{FailureThreshold: 1, SendRecovery: true},
		Endpoints: []config.EndpointConfig{endpoint},
	}
	sender := &recordingSender{}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	runner := NewRunner(cfg, nil, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}
	now := time.Now()

	runner.processResult(context.Background(), endpoint, &state, check.Result{Err: errors.New("worker:office stale 7m")}, now)
	runner.processResult(context.Background(), endpoint, &state, check.Result{Healthy: true}, now.Add(time.Minute))
	if len(sender.messages) != 2 {
		t.Fatalf("sent %d messages, want outage and recovery", len(sender.messages))
	}
	if body := sender.messages[0].Body; !strings.Contains(body, "Reason: worker:office stale 7m") {
		t.Errorf("outage body does not contain the query detail:\n%s", body)
	}
	if body := sender.messages[1].Body; strings.Contains(body, "HTTP status") {
		t.Errorf("recovery body for a database check mentions HTTP status:\n%s", body)
	}
}

func TestAlertsAndStatusRedactURLQuery(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com/health?token=s3cret"}
	cfg := config.Config{
		Alerts:    config.AlertConfig{FailureThreshold: 1, SendRecovery: true},
		Endpoints: []config.EndpointConfig{endpoint},
	}
	sender := &recordingSender{}
	store := NewStatusStore(cfg.Endpoints)
	t.Cleanup(func() { _ = store.Close() })
	runner := NewRunner(cfg, nil, sender, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	state := endpointState{}
	now := time.Now()

	runner.processResult(context.Background(), endpoint, &state, check.Result{Err: errors.New("timeout")}, now)
	runner.processResult(context.Background(), endpoint, &state, check.Result{Healthy: true, StatusCode: 200}, now.Add(time.Second))
	if len(sender.messages) != 2 {
		t.Fatalf("sent %d messages, want outage and recovery", len(sender.messages))
	}
	for _, message := range sender.messages {
		if strings.Contains(message.Body, "s3cret") || !strings.Contains(message.Body, "token=REDACTED") {
			t.Fatalf("email body does not redact the query:\n%s", message.Body)
		}
	}

	statuses, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].URL != "https://example.com/health?token=REDACTED" {
		t.Fatalf("status URL = %q, want query redacted", statuses[0].URL)
	}
}
