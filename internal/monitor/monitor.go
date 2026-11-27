package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/qbite-tech/healthz/internal/alert"
	"github.com/qbite-tech/healthz/internal/check"
	"github.com/qbite-tech/healthz/internal/config"
)

type Runner struct {
	config  config.Config
	checker check.Checker
	sender  alert.Sender
	status  *StatusStore
	logger  *slog.Logger
	jitter  func(time.Duration) time.Duration
	// wake lets a received ping trigger its endpoint's next check early.
	wake map[string]chan struct{}
}

type endpointState struct {
	failures      int
	alerted       bool
	lastSent      time.Time
	outageStarted time.Time
}

func NewRunner(cfg config.Config, checker check.Checker, sender alert.Sender, status *StatusStore, logger *slog.Logger) *Runner {
	wake := make(map[string]chan struct{})
	for _, endpoint := range cfg.Endpoints {
		if endpoint.Type == config.EndpointTypePing {
			wake[endpoint.Name] = make(chan struct{}, 1)
		}
	}
	return &Runner{config: cfg, checker: checker, sender: sender, status: status, logger: logger, jitter: randomJitter, wake: wake}
}

// Wake runs the named ping endpoint's next check now instead of at its next
// scheduled time. Wakes that arrive while a check is pending are coalesced.
func (r *Runner) Wake(endpointName string) {
	channel, ok := r.wake[endpointName]
	if !ok {
		return
	}
	select {
	case channel <- struct{}{}:
	default:
	}
}

func (r *Runner) waitForNextCheck(ctx context.Context, endpoint config.EndpointConfig, delay time.Duration) bool {
	timer := time.NewTimer(max(0, delay))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-r.wake[endpoint.Name]: // nil channel for non-ping endpoints never fires
		return true
	}
}

func (r *Runner) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for _, endpoint := range r.config.Endpoints {
		workers.Add(1)
		go func() {
			defer workers.Done()
			r.runEndpoint(ctx, endpoint)
		}()
	}
	workers.Wait()
}

func (r *Runner) runEndpoint(ctx context.Context, endpoint config.EndpointConfig) {
	state := endpointState{}
	if !r.waitForNextCheck(ctx, endpoint, r.jitter(endpoint.ScheduleJitter.Duration)) {
		return
	}
	for {
		r.runCheckCycle(ctx, endpoint, &state)
		if ctx.Err() != nil {
			return
		}
		if !r.waitForNextCheck(ctx, endpoint, r.nextCheckDelay(endpoint)) {
			return
		}
	}
}

func (r *Runner) runCheckCycle(ctx context.Context, endpoint config.EndpointConfig, state *endpointState) {
	var prior endpointState
	canConfirm := endpoint.RecheckBeforeAlert
	if canConfirm {
		var err error
		prior, err = r.status.EndpointState(ctx, endpoint.Name)
		if err != nil {
			r.logger.Error("could not load endpoint state before check", "endpoint", endpoint.Name, "error", err)
			canConfirm = false
		}
	}

	result := r.checker.Check(ctx, endpoint)
	if ctx.Err() != nil {
		return
	}
	shouldConfirm := canConfirm && !result.Healthy && !prior.alerted &&
		prior.failures+1 >= r.config.ThresholdFor(endpoint)
	if !shouldConfirm {
		r.processResult(ctx, endpoint, state, result, time.Now())
		return
	}

	r.processResultWithOptions(ctx, endpoint, state, result, time.Now(), true)
	if ctx.Err() != nil {
		return
	}
	r.logger.Info("rechecking endpoint before declaring outage", "endpoint", endpoint.Name)
	confirmation := r.checker.Check(ctx, endpoint)
	if ctx.Err() != nil {
		return
	}
	r.processResult(ctx, endpoint, state, confirmation, time.Now())
}

func (r *Runner) processResult(ctx context.Context, endpoint config.EndpointConfig, state *endpointState, result check.Result, checkedAt time.Time) {
	r.processResultWithOptions(ctx, endpoint, state, result, checkedAt, false)
}

func (r *Runner) processResultWithOptions(ctx context.Context, endpoint config.EndpointConfig, state *endpointState, result check.Result, checkedAt time.Time, suppressFailureAlert bool) {
	persisted, err := r.status.RecordCheck(ctx, endpoint, result, checkedAt)
	if err != nil {
		r.logger.Error("could not persist endpoint check", "endpoint", endpoint.Name, "error", err)
		return
	}
	*state = persisted
	r.evaluateFlapping(ctx, endpoint, checkedAt)

	if result.Healthy {
		wasAlerted := state.alerted
		r.logger.Info("endpoint is healthy", "endpoint", endpoint.Name, "status", result.StatusCode, "latency", result.Duration)
		if wasAlerted && r.config.Alerts.SendRecovery {
			httpStatus := ""
			if result.StatusCode != 0 {
				httpStatus = fmt.Sprintf("HTTP status: %d\n", result.StatusCode)
			}
			message := alert.Message{
				Subject: renderAlertSubject(endpoint.AlertSubjects.Recovered,
					fmt.Sprintf("[RECOVERED] %s is healthy", endpoint.Name),
					map[string]string{"name": endpoint.Name, "status": fmt.Sprintf("%d", result.StatusCode)}),
				Recipients: endpoint.AlertTo,
				Body: fmt.Sprintf("Endpoint: %s\nURL: %s\nRecovered at: %s\n%sLatency: %s\n",
					endpoint.Name, config.RedactURL(endpoint.URL), checkedAt.Format(time.RFC3339), httpStatus, result.Duration.Round(time.Millisecond)),
			}
			if err := r.send(ctx, message); err != nil {
				r.logger.Error("could not send recovery email", "endpoint", endpoint.Name, "error", err)
				return
			}
			r.logger.Info("sent recovery email", "endpoint", endpoint.Name)
		}
		if wasAlerted {
			if err := r.status.ClearAlert(ctx, endpoint.Name, checkedAt); err != nil {
				r.logger.Error("recovery email was processed but recovery state could not be persisted", "endpoint", endpoint.Name, "error", err)
				return
			}
		}
		state.alerted = false
		state.lastSent = time.Time{}
		return
	}

	r.logger.Warn("endpoint check failed", "endpoint", endpoint.Name, "failures", state.failures, "error", result.Err)
	if suppressFailureAlert {
		return
	}

	threshold := r.config.ThresholdFor(endpoint)
	if state.failures < threshold {
		return
	}
	shouldSend := !state.alerted
	if state.alerted && r.config.Alerts.RepeatInterval.Duration > 0 {
		shouldSend = checkedAt.Sub(state.lastSent) >= r.config.Alerts.RepeatInterval.Duration
	}
	if !shouldSend {
		return
	}

	reason := "unknown failure"
	if result.Err != nil {
		reason = result.Err.Error()
	}
	outageStarted := "unknown"
	if !state.outageStarted.IsZero() {
		outageStarted = state.outageStarted.Format(time.RFC3339)
	}
	message := alert.Message{
		Subject: renderAlertSubject(endpoint.AlertSubjects.Down,
			fmt.Sprintf("[DOWN] %s health check failed", endpoint.Name),
			map[string]string{"name": endpoint.Name, "failures": fmt.Sprintf("%d", state.failures)}),
		Recipients: endpoint.AlertTo,
		Body: fmt.Sprintf("Endpoint: %s\nURL: %s\nOutage started: %s\nChecked at: %s\nConsecutive failures: %d\nReason: %s\nLatency: %s\n",
			endpoint.Name, config.RedactURL(endpoint.URL), outageStarted, checkedAt.Format(time.RFC3339), state.failures, reason, result.Duration.Round(time.Millisecond)),
	}
	if err := r.send(ctx, message); err != nil {
		r.logger.Error("could not send failure email", "endpoint", endpoint.Name, "error", err)
		return
	}
	if err := r.status.MarkAlertSent(ctx, endpoint.Name, checkedAt); err != nil {
		r.logger.Error("failure email was sent but alert state could not be persisted", "endpoint", endpoint.Name, "error", err)
		return
	}
	state.alerted = true
	state.lastSent = checkedAt
	r.logger.Info("sent failure email", "endpoint", endpoint.Name)
}

func (r *Runner) evaluateFlapping(ctx context.Context, endpoint config.EndpointConfig, checkedAt time.Time) {
	if !endpoint.Flapping.Enabled {
		return
	}
	evaluation, err := r.status.EvaluateFlapping(ctx, endpoint.Name, checkedAt,
		endpoint.Flapping.Window.Duration, endpoint.Flapping.Transitions)
	if err != nil {
		r.logger.Error("could not evaluate endpoint flapping", "endpoint", endpoint.Name, "error", err)
		return
	}
	if evaluation.BecameStable {
		r.logger.Info("endpoint is no longer flapping", "endpoint", endpoint.Name)
	}
	if !evaluation.NeedsAlert {
		return
	}
	message := alert.Message{
		Subject: renderAlertSubject(endpoint.AlertSubjects.Flapping,
			fmt.Sprintf("[FLAPPING] %s is rapidly changing state", endpoint.Name),
			map[string]string{
				"name": endpoint.Name, "transitions": fmt.Sprintf("%d", evaluation.Transitions),
				"window": endpoint.Flapping.Window.Duration.String(),
			}),
		Recipients: endpoint.AlertTo,
		Body: fmt.Sprintf("Endpoint: %s\nURL: %s\nDetected at: %s\nState changes: %d within %s\nCurrent checks are rapidly alternating between healthy and unhealthy.\n",
			endpoint.Name, config.RedactURL(endpoint.URL), checkedAt.Format(time.RFC3339), evaluation.Transitions,
			endpoint.Flapping.Window.Duration),
	}
	if err := r.send(ctx, message); err != nil {
		r.logger.Error("could not send flapping email", "endpoint", endpoint.Name, "error", err)
		return
	}
	if err := r.status.MarkFlapAlertSent(ctx, endpoint.Name, checkedAt); err != nil {
		r.logger.Error("flapping email was sent but alert state could not be persisted", "endpoint", endpoint.Name, "error", err)
		return
	}
	r.logger.Info("sent flapping email", "endpoint", endpoint.Name)
}

func (r *Runner) nextCheckDelay(endpoint config.EndpointConfig) time.Duration {
	return endpoint.Frequency.Duration + r.jitter(endpoint.ScheduleJitter.Duration)
}

func randomJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maximum)))
}

func renderAlertSubject(template, fallback string, values map[string]string) string {
	if template == "" {
		return fallback
	}
	replacements := make([]string, 0, len(values)*2)
	for placeholder, value := range values {
		replacements = append(replacements, "{"+placeholder+"}", value)
	}
	return strings.NewReplacer(replacements...).Replace(template)
}

func (r *Runner) send(parent context.Context, message alert.Message) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	return r.sender.Send(ctx, message)
}
