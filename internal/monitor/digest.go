package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/runeQ/healthz/internal/alert"
	"github.com/runeQ/healthz/internal/config"
)

const digestRetryInterval = 15 * time.Minute

type DigestRunner struct {
	config config.DigestConfig
	store  *StatusStore
	sender alert.Sender
	logger *slog.Logger
}

func NewDigestRunner(cfg config.DigestConfig, store *StatusStore, sender alert.Sender, logger *slog.Logger) *DigestRunner {
	return &DigestRunner{config: cfg, store: store, sender: sender, logger: logger}
}

func (r *DigestRunner) Run(ctx context.Context) {
	location, err := time.LoadLocation(r.config.Timezone)
	if err != nil {
		r.logger.Error("could not load digest timezone", "timezone", r.config.Timezone, "error", err)
		return
	}

	for {
		next, err := nextDigestTime(time.Now(), r.config.SendAt, location)
		if err != nil {
			r.logger.Error("could not schedule status digest", "error", err)
			return
		}
		r.logger.Info("daily status digest scheduled", "send_at", next)
		if !waitUntil(ctx, next) {
			return
		}

		for {
			if err := r.sendDigest(ctx, time.Now()); err == nil {
				r.logger.Info("sent daily status digest")
				break
			} else {
				r.logger.Error("could not send daily status digest; will retry", "retry_in", digestRetryInterval, "error", err)
			}
			if !waitFor(ctx, digestRetryInterval) {
				return
			}
		}
	}
}

func (r *DigestRunner) sendDigest(parent context.Context, now time.Time) error {
	snapshot, err := r.store.DigestSnapshot(parent, now)
	if err != nil {
		return err
	}
	message := buildDigestMessage(snapshot)
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := r.sender.Send(ctx, message); err != nil {
		return err
	}
	return r.store.CommitDigest(ctx, snapshot)
}

func nextDigestTime(now time.Time, sendAt string, location *time.Location) (time.Time, error) {
	clock, err := time.Parse("15:04", sendAt)
	if err != nil {
		return time.Time{}, err
	}
	localNow := now.In(location)
	next := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), clock.Hour(), clock.Minute(), 0, 0, location)
	if !next.After(localNow) {
		next = time.Date(localNow.Year(), localNow.Month(), localNow.Day()+1, clock.Hour(), clock.Minute(), 0, 0, location)
	}
	return next, nil
}

func waitUntil(ctx context.Context, target time.Time) bool {
	return waitFor(ctx, time.Until(target))
}

func waitFor(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(max(0, duration))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func buildDigestMessage(snapshot DigestSnapshot) alert.Message {
	healthy, unhealthy, unknown := 0, 0, 0
	for _, endpoint := range snapshot.Endpoints {
		switch {
		case !endpoint.Status.Known:
			unknown++
		case endpoint.Status.Healthy:
			healthy++
		default:
			unhealthy++
		}
	}

	var body strings.Builder
	fmt.Fprintln(&body, "Daily website health report")
	fmt.Fprintln(&body)
	fmt.Fprintf(&body, "Report period: %s to %s\n", snapshot.From.Format(time.RFC3339), snapshot.To.Format(time.RFC3339))
	fmt.Fprintf(&body, "Monitor uptime: %s\n", formatUptime(snapshot.To.Sub(snapshot.StartedAt)))
	fmt.Fprintf(&body, "Current status: %d healthy, %d unhealthy, %d awaiting first check\n", healthy, unhealthy, unknown)

	for _, endpoint := range snapshot.Endpoints {
		status := "UNKNOWN"
		if endpoint.Status.Known && endpoint.Status.Healthy {
			status = "UP"
		} else if endpoint.Status.Known {
			status = "DOWN"
		}
		fmt.Fprintf(&body, "\n[%s] %s\n", status, endpoint.Status.Name)
		fmt.Fprintf(&body, "URL: %s\n", endpoint.Status.URL)
		fmt.Fprintf(&body, "Checks: %d passed, %d failed, %d total\n", endpoint.Stats.PassedChecks, endpoint.Stats.FailedChecks, endpoint.Stats.Checks)
		if endpoint.Stats.Checks > 0 {
			availability := float64(endpoint.Stats.PassedChecks) / float64(endpoint.Stats.Checks) * 100
			fmt.Fprintf(&body, "Check availability: %.2f%%\n", availability)
		} else {
			fmt.Fprintln(&body, "Check availability: no checks in this period")
		}
		if endpoint.Status.Known {
			fmt.Fprintf(&body, "Last checked: %s\n", endpoint.Status.LastChecked.Format(time.RFC3339))
			fmt.Fprintf(&body, "Last latency: %d ms\n", endpoint.Status.LastLatencyMillis)
		}
		if endpoint.Status.LastError != "" {
			fmt.Fprintf(&body, "Current error: %s\n", endpoint.Status.LastError)
		}
		if endpoint.Status.Flapping {
			fmt.Fprintln(&body, "Flapping: yes")
		}
	}

	return alert.Message{
		Subject: fmt.Sprintf("[STATUS] Daily health report: %d/%d healthy", healthy, len(snapshot.Endpoints)),
		Body:    body.String(),
	}
}

func formatUptime(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}
