package check

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/runeQ/healthz/internal/config"
)

// PingState is what a ping endpoint has received from the job it watches.
// Zero times mean the event has not happened.
type PingState struct {
	// Baseline is when healthz began expecting pings, so a job that never
	// reports at all is still noticed.
	Baseline    time.Time
	LastSuccess time.Time
	LastStart   time.Time
	LastFail    time.Time
}

// PingStateSource loads the pings recorded for an endpoint.
type PingStateSource interface {
	PingState(ctx context.Context, endpointName string) (PingState, error)
}

// PingChecker evaluates recorded pings against the endpoint's schedule. It
// makes no network requests; jobs report to healthz's ping address instead.
type PingChecker struct {
	source PingStateSource
	now    func() time.Time
}

func NewPingChecker(source PingStateSource) *PingChecker {
	return &PingChecker{source: source, now: time.Now}
}

func (c *PingChecker) Check(ctx context.Context, endpoint config.EndpointConfig) Result {
	started := time.Now()
	if c.source == nil {
		return Result{Err: errors.New("ping state is unavailable")}
	}
	state, err := c.source.PingState(ctx, endpoint.Name)
	if err != nil {
		return Result{Duration: time.Since(started), Err: fmt.Errorf("load ping state: %w", err)}
	}
	schedule, err := endpoint.PingSchedule()
	if err != nil {
		return Result{Duration: time.Since(started), Err: err}
	}
	result := EvaluatePing(state, schedule, endpoint.Grace.Duration, c.now())
	result.Duration = time.Since(started)
	return result
}

// EvaluatePing decides whether a job is healthy from its pings:
//   - a failure report stays unhealthy until the next success report;
//   - a started run is healthy for grace, then unhealthy until it reports;
//   - otherwise the next ping is due at the first scheduled time after the last
//     success (or the baseline), and is missing once grace has also passed.
func EvaluatePing(state PingState, schedule config.PingSchedule, grace time.Duration, now time.Time) Result {
	if !state.LastFail.IsZero() && !state.LastFail.Before(state.LastSuccess) {
		return Result{Err: fmt.Errorf("job reported failure at %s", formatPingTime(state.LastFail))}
	}
	if state.LastStart.After(state.LastSuccess) {
		if now.Sub(state.LastStart) > grace {
			return Result{Err: fmt.Errorf("job started at %s but did not report completion within %s",
				formatPingTime(state.LastStart), grace)}
		}
		return Result{Healthy: true}
	}

	reference := state.LastSuccess
	if reference.Before(state.Baseline) {
		reference = state.Baseline
	}
	if reference.IsZero() {
		return Result{Healthy: true}
	}
	due := schedule.Next(reference)
	if due.IsZero() || !now.After(due.Add(grace)) {
		return Result{Healthy: true}
	}
	if state.LastSuccess.IsZero() {
		return Result{Err: fmt.Errorf("no ping received; one was expected by %s (plus %s grace)",
			formatPingTime(due), grace)}
	}
	return Result{Err: fmt.Errorf("no ping since %s; the next was expected by %s (plus %s grace)",
		formatPingTime(state.LastSuccess), formatPingTime(due), grace)}
}

func formatPingTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
