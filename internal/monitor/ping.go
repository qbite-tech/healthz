package monitor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/qbite-tech/healthz/internal/check"
	"github.com/qbite-tech/healthz/internal/config"
)

// PingKind is the event a job reports to its ping address.
type PingKind int

const (
	PingSuccess PingKind = iota
	PingStart
	PingFail
)

func (k PingKind) String() string {
	switch k {
	case PingStart:
		return "start"
	case PingFail:
		return "fail"
	default:
		return "success"
	}
}

func (k PingKind) column() string {
	switch k {
	case PingStart:
		return "last_ping_start_ns"
	case PingFail:
		return "last_ping_fail_ns"
	default:
		return "last_ping_success_ns"
	}
}

const (
	minPingKeyLength = 32
	maxPingKeyLength = 128
)

var pingKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// PingKeys maps the SHA-256 of each secret ping key to its endpoint name.
type PingKeys map[[32]byte]string

// NewPingKeys reads the key of every ping endpoint through lookup and rejects
// keys that are missing, too short to be unguessable, unsafe in a URL path, or
// shared between endpoints.
func NewPingKeys(endpoints []config.EndpointConfig, lookup func(string) (string, bool)) (PingKeys, error) {
	keys := make(PingKeys)
	for _, endpoint := range endpoints {
		if endpoint.Type != config.EndpointTypePing {
			continue
		}
		key, exists := lookup(endpoint.PingKeyEnv)
		if !exists || key == "" {
			return nil, fmt.Errorf("ping key environment variable %s for %q is not set", endpoint.PingKeyEnv, endpoint.Name)
		}
		if len(key) < minPingKeyLength || len(key) > maxPingKeyLength || !pingKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("ping key in %s for %q must be %d to %d letters, digits, '-' or '_'; generate one with `openssl rand -hex 16`",
				endpoint.PingKeyEnv, endpoint.Name, minPingKeyLength, maxPingKeyLength)
		}
		hash := sha256.Sum256([]byte(key))
		if other, exists := keys[hash]; exists {
			return nil, fmt.Errorf("ping endpoints %q and %q use the same key", other, endpoint.Name)
		}
		keys[hash] = endpoint.Name
	}
	return keys, nil
}

// RecordPing stores a ping received from the job behind endpointName.
func (s *StatusStore) RecordPing(ctx context.Context, endpointName string, kind PingKind, at time.Time) error {
	result, err := s.db.ExecContext(ctx, "UPDATE endpoints SET "+kind.column()+" = ? WHERE name = ? AND enabled = 1",
		at.UnixNano(), endpointName)
	if err != nil {
		return fmt.Errorf("persist %s ping: %w", kind, err)
	}
	return requireUpdated(result, endpointName)
}

// PingState implements check.PingStateSource.
func (s *StatusStore) PingState(ctx context.Context, endpointName string) (check.PingState, error) {
	var baseline, success, start, fail sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT ping_baseline_ns, last_ping_success_ns, last_ping_start_ns,
		last_ping_fail_ns FROM endpoints WHERE name = ? AND enabled = 1`, endpointName).
		Scan(&baseline, &success, &start, &fail); err != nil {
		return check.PingState{}, fmt.Errorf("load ping state %q: %w", endpointName, err)
	}
	return check.PingState{
		Baseline:    nullTimeValue(baseline),
		LastSuccess: nullTimeValue(success),
		LastStart:   nullTimeValue(start),
		LastFail:    nullTimeValue(fail),
	}, nil
}

// PingHandler serves the unauthenticated ping addresses:
//
//	/ping/<key>         the run succeeded
//	/ping/<key>/start   the run started
//	/ping/<key>/fail    the run failed
//	/ping/<key>/<code>  exit status: 0 succeeded, 1-255 failed
//
// The secret key in the path is the credential, as with healthchecks.io. After
// a ping is stored, notify is called with the endpoint name so it can be
// evaluated without waiting for the next scheduled check.
func (s *StatusStore) PingHandler(keys PingKeys, notify func(string), logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	rejections := &logLimiter{interval: rejectedPingLogInterval}
	handle := func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet, http.MethodHead, http.MethodPost:
		default:
			writer.Header().Set("Allow", "GET, HEAD, POST")
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		kind, exitCode, ok := parsePingAction(request.PathValue("action"))
		if !ok {
			http.NotFound(writer, request)
			return
		}
		endpointName, exists := keys[sha256.Sum256([]byte(request.PathValue("key")))]
		if !exists {
			// The path is public, so bound the log volume an anonymous client can
			// cause. Never log the key: a near-miss may still be a valid secret.
			if shouldLog, suppressed := rejections.record(time.Now()); shouldLog {
				logger.Warn("rejected ping with unknown key", "remote_addr", request.RemoteAddr,
					"suppressed_since_last_log", suppressed)
			}
			http.NotFound(writer, request)
			return
		}
		receivedAt := time.Now()
		if err := s.RecordPing(request.Context(), endpointName, kind, receivedAt); err != nil {
			logger.Error("could not record ping", "endpoint", endpointName, "kind", kind, "error", err)
			status := http.StatusInternalServerError
			if errors.Is(err, context.Canceled) {
				status = http.StatusServiceUnavailable
			}
			http.Error(writer, "could not record ping", status)
			return
		}
		attributes := []any{"endpoint", endpointName, "kind", kind}
		if exitCode >= 0 {
			attributes = append(attributes, "exit_code", exitCode)
		}
		logger.Info("received ping", attributes...)
		if notify != nil {
			notify(endpointName)
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte("OK\n"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping/{key}", handle)
	mux.HandleFunc("/ping/{key}/{action}", handle)
	return mux
}

// rejectedPingLogInterval is the minimum time between log lines about pings
// with unknown keys.
const rejectedPingLogInterval = time.Minute

// logLimiter allows one log line per interval and counts the events it
// suppressed in between, so a flood stays visible without filling the log.
type logLimiter struct {
	mu         sync.Mutex
	interval   time.Duration
	lastLogged time.Time
	suppressed int
}

// record notes an event at now. It reports whether the event should be logged
// and, if so, how many events were suppressed since the previous log line.
func (l *logLimiter) record(now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.lastLogged.IsZero() && now.Sub(l.lastLogged) < l.interval {
		l.suppressed++
		return false, 0
	}
	suppressed := l.suppressed
	l.lastLogged, l.suppressed = now, 0
	return true, suppressed
}

func parsePingAction(action string) (PingKind, int, bool) {
	switch action {
	case "":
		return PingSuccess, -1, true
	case "start":
		return PingStart, -1, true
	case "fail":
		return PingFail, -1, true
	}
	code, err := strconv.Atoi(action)
	if err != nil || code < 0 || code > 255 || strconv.Itoa(code) != action {
		return 0, 0, false
	}
	if code == 0 {
		return PingSuccess, code, true
	}
	return PingFail, code, true
}
