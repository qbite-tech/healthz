package check

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/runeQ/healthz/internal/config"
)

func dailyBackupEndpoint(t *testing.T) config.EndpointConfig {
	t.Helper()
	return config.EndpointConfig{
		Name: "backup", Type: config.EndpointTypePing, PingKeyEnv: "BACKUP_PING_KEY",
		Schedule: "30 2 * * *", Timezone: "UTC", Grace: config.Duration{Duration: 3 * time.Hour},
	}
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestEvaluatePingWithCronSchedule(t *testing.T) {
	endpoint := dailyBackupEndpoint(t)
	schedule, err := endpoint.PingSchedule()
	if err != nil {
		t.Fatal(err)
	}
	baseline := at(t, "2026-11-01T10:00:00Z")
	success := at(t, "2026-11-02T02:50:00Z")
	for name, test := range map[string]struct {
		state   PingState
		now     string
		healthy bool
		reason  string
	}{
		"waiting for first run":    {PingState{Baseline: baseline}, "2026-11-02T05:29:00Z", true, ""},
		"first run never reported": {PingState{Baseline: baseline}, "2026-11-02T05:31:00Z", false, "no ping received; one was expected by 2026-11-02T02:30:00Z"},
		"ran on schedule":          {PingState{Baseline: baseline, LastSuccess: success}, "2026-11-03T05:29:00Z", true, ""},
		"missed next run":          {PingState{Baseline: baseline, LastSuccess: success}, "2026-11-03T05:31:00Z", false, "no ping since 2026-11-02T02:50:00Z; the next was expected by 2026-11-03T02:30:00Z"},
		"reported failure": {
			PingState{Baseline: baseline, LastSuccess: success, LastFail: at(t, "2026-11-03T02:45:00Z")},
			"2026-11-03T02:46:00Z", false, "job reported failure at 2026-11-03T02:45:00Z",
		},
		"failure cleared by later success": {
			PingState{Baseline: baseline, LastFail: success.Add(-time.Hour), LastSuccess: success},
			"2026-11-02T03:00:00Z", true, "",
		},
		"run in progress past schedule": {
			PingState{Baseline: baseline, LastSuccess: success, LastStart: at(t, "2026-11-03T05:00:00Z")},
			"2026-11-03T07:59:00Z", true, "",
		},
		"run never finished": {
			PingState{Baseline: baseline, LastSuccess: success, LastStart: at(t, "2026-11-03T02:30:00Z")},
			"2026-11-03T05:31:00Z", false, "job started at 2026-11-03T02:30:00Z but did not report completion within 3h0m0s",
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := EvaluatePing(test.state, schedule, endpoint.Grace.Duration, at(t, test.now))
			if result.Healthy != test.healthy {
				t.Fatalf("healthy = %v, want %v (err %v)", result.Healthy, test.healthy, result.Err)
			}
			if test.reason != "" && (result.Err == nil || !strings.Contains(result.Err.Error(), test.reason)) {
				t.Fatalf("err = %v, want it to contain %q", result.Err, test.reason)
			}
		})
	}
}

func TestEvaluatePingWithPeriod(t *testing.T) {
	endpoint := config.EndpointConfig{Period: config.Duration{Duration: time.Hour}, Grace: config.Duration{Duration: 10 * time.Minute}}
	schedule, err := endpoint.PingSchedule()
	if err != nil {
		t.Fatal(err)
	}
	success := at(t, "2026-11-02T12:00:00Z")
	state := PingState{Baseline: success.Add(-24 * time.Hour), LastSuccess: success}
	if result := EvaluatePing(state, schedule, endpoint.Grace.Duration, success.Add(70*time.Minute)); !result.Healthy {
		t.Fatalf("within period plus grace: %v", result.Err)
	}
	if result := EvaluatePing(state, schedule, endpoint.Grace.Duration, success.Add(71*time.Minute)); result.Healthy {
		t.Fatal("past period plus grace was healthy")
	}
}

type staticPingSource struct {
	state PingState
	err   error
}

func (s staticPingSource) PingState(context.Context, string) (PingState, error) {
	return s.state, s.err
}

func TestPingCheckerUsesSourceAndClock(t *testing.T) {
	endpoint := dailyBackupEndpoint(t)
	checker := NewPingChecker(staticPingSource{state: PingState{Baseline: at(t, "2026-11-01T10:00:00Z")}})
	checker.now = func() time.Time { return at(t, "2026-11-04T00:00:00Z") }
	if result := checker.Check(context.Background(), endpoint); result.Healthy || result.Err == nil {
		t.Fatalf("result = %+v, want missing ping", result)
	}

	failing := NewPingChecker(staticPingSource{err: errors.New("disk I/O error")})
	if result := failing.Check(context.Background(), endpoint); result.Healthy || !strings.Contains(result.Err.Error(), "disk I/O error") {
		t.Fatalf("result = %+v, want source error", result)
	}
	if result := NewPingChecker(nil).Check(context.Background(), endpoint); result.Healthy {
		t.Fatal("checker without a source reported healthy")
	}
}
