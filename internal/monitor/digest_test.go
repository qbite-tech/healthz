package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/runeQ/healthz/internal/check"
	"github.com/runeQ/healthz/internal/config"
)

func TestNextDigestTimeUsesConfiguredTimezone(t *testing.T) {
	location, err := time.LoadLocation("Europe/Copenhagen")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 11, 5, 30, 0, 0, time.UTC) // 07:30 in Copenhagen.

	next, err := nextDigestTime(now, "08:00", location)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.August, 11, 8, 0, 0, 0, location)
	if !next.Equal(want) {
		t.Fatalf("next digest = %s, want %s", next, want)
	}

	next, err = nextDigestTime(want, "08:00", location)
	if err != nil {
		t.Fatal(err)
	}
	want = time.Date(2026, time.August, 12, 8, 0, 0, 0, location)
	if !next.Equal(want) {
		t.Fatalf("next digest after send time = %s, want %s", next, want)
	}
}

func TestDigestContainsStatusAndPeriodStats(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "Main website", URL: "https://example.com"}
	store := NewStatusStore([]config.EndpointConfig{endpoint})
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true, StatusCode: 200, Duration: 20 * time.Millisecond}, now)
	mustRecordCheck(t, store, endpoint, check.Result{Err: errors.New("timeout"), Duration: time.Second}, now.Add(time.Minute))

	message := buildDigestMessage(mustDigestSnapshot(t, store, now.Add(2*time.Minute)))
	if len(message.Recipients) != 0 {
		t.Fatalf("daily digest has endpoint recipients %v, want global fallback", message.Recipients)
	}
	for _, expected := range []string{
		"[STATUS] Daily health report: 0/1 healthy",
		"[DOWN] Main website",
		"Checks: 1 passed, 1 failed, 2 total",
		"Check availability: 50.00%",
		"Current error: timeout",
	} {
		if !strings.Contains(message.Subject+"\n"+message.Body, expected) {
			t.Errorf("digest does not contain %q:\n%s\n%s", expected, message.Subject, message.Body)
		}
	}
}

func TestDigestCommitsStatsOnlyAfterSuccessfulDelivery(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	store := NewStatusStore([]config.EndpointConfig{endpoint})
	t.Cleanup(func() { _ = store.Close() })
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true, StatusCode: 200}, time.Now())
	sender := &recordingSender{err: errors.New("SMTP unavailable")}
	runner := NewDigestRunner(config.DigestConfig{}, store, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := runner.sendDigest(context.Background(), time.Now()); err == nil {
		t.Fatal("sendDigest() succeeded while SMTP sender failed")
	}
	if got := mustDigestSnapshot(t, store, time.Now()).Endpoints[0].Stats.Checks; got != 1 {
		t.Fatalf("checks after failed delivery = %d, want 1", got)
	}

	sender.err = nil
	if err := runner.sendDigest(context.Background(), time.Now()); err != nil {
		t.Fatalf("sendDigest() error = %v", err)
	}
	if got := mustDigestSnapshot(t, store, time.Now()).Endpoints[0].Stats.Checks; got != 0 {
		t.Fatalf("checks after successful delivery = %d, want 0", got)
	}
}

func TestDigestCommitPreservesConcurrentChecks(t *testing.T) {
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	store := NewStatusStore([]config.EndpointConfig{endpoint})
	t.Cleanup(func() { _ = store.Close() })
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true}, time.Now())
	snapshot := mustDigestSnapshot(t, store, time.Now())
	mustRecordCheck(t, store, endpoint, check.Result{Healthy: true}, time.Now())

	if err := store.CommitDigest(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	stats := mustDigestSnapshot(t, store, time.Now()).Endpoints[0].Stats
	if stats.Checks != 1 || stats.PassedChecks != 1 {
		t.Fatalf("stats after concurrent commit = %+v, want one retained check", stats)
	}
}

func mustRecordCheck(t *testing.T, store *StatusStore, endpoint config.EndpointConfig, result check.Result, checkedAt time.Time) endpointState {
	t.Helper()
	state, err := store.RecordCheck(context.Background(), endpoint, result, checkedAt)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func mustDigestSnapshot(t *testing.T, store *StatusStore, at time.Time) DigestSnapshot {
	t.Helper()
	snapshot, err := store.DigestSnapshot(context.Background(), at)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
