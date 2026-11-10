package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// Duration accepts human-readable JSON strings such as "30s" or "5m".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("duration must be a string such as \"30s\"")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	d.Duration = parsed
	return nil
}

type Config struct {
	Server    ServerConfig     `json:"server"`
	SMTP      SMTPConfig       `json:"smtp"`
	Alerts    AlertConfig      `json:"alerts"`
	Digest    DigestConfig     `json:"status_digest"`
	Storage   StorageConfig    `json:"storage"`
	Endpoints []EndpointConfig `json:"endpoints"`
}

type ServerConfig struct {
	Listen       string `json:"listen"`
	AuthTokenEnv string `json:"auth_token_env"`
}

type SMTPConfig struct {
	Enabled     bool     `json:"enabled"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	TLS         string   `json:"tls"`
	Username    string   `json:"username"`
	PasswordEnv string   `json:"password_env"`
	From        string   `json:"from"`
	To          []string `json:"to"`
}

type AlertConfig struct {
	FailureThreshold int      `json:"failure_threshold"`
	SendRecovery     bool     `json:"send_recovery"`
	RepeatInterval   Duration `json:"repeat_interval"`
}

type DigestConfig struct {
	Enabled  bool   `json:"enabled"`
	SendAt   string `json:"send_at"`
	Timezone string `json:"timezone"`
}

type StorageConfig struct {
	Path                 string `json:"path"`
	HistoryRetentionDays int    `json:"history_retention_days"`
}

const (
	EndpointTypeHTTP     = "http"
	EndpointTypePostgres = "postgres"
	EndpointTypePing     = "ping"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type EndpointConfig struct {
	Name string `json:"name"`
	// Type selects the checker: "http" (default), "postgres", or "ping".
	Type string `json:"type"`
	// URL is the HTTP target. For postgres and ping checks it is derived from
	// DSNEnv or PingKeyEnv and only identifies the target in status, history,
	// and alerts.
	URL    string `json:"url"`
	DSNEnv string `json:"dsn_env"`
	Query  string `json:"query"`
	// Ping endpoints are passive: a job reports to /ping/<key>, and the check
	// fails when the job reports failure or an expected ping is missing.
	PingKeyEnv         string             `json:"ping_key_env"`
	Schedule           string             `json:"schedule"`
	Timezone           string             `json:"timezone"`
	Period             Duration           `json:"period"`
	Grace              Duration           `json:"grace"`
	Method             string             `json:"method"`
	Frequency          Duration           `json:"frequency"`
	ScheduleJitter     Duration           `json:"schedule_jitter"`
	Timeout            Duration           `json:"timeout"`
	ExpectedStatuses   []int              `json:"expected_statuses"`
	BodyContains       string             `json:"body_contains"`
	Headers            map[string]string  `json:"headers"`
	FailureThreshold   int                `json:"failure_threshold"`
	RecheckBeforeAlert bool               `json:"recheck_before_alert"`
	Flapping           FlappingConfig     `json:"flapping"`
	AlertSubjects      AlertSubjectConfig `json:"alert_subjects"`
	AlertTo            []string           `json:"alert_to"`
}

type FlappingConfig struct {
	Enabled     bool     `json:"enabled"`
	Transitions int      `json:"transitions"`
	Window      Duration `json:"window"`
}

type AlertSubjectConfig struct {
	Down      string `json:"down"`
	Recovered string `json:"recovered"`
	Flapping  string `json:"flapping"`
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Config{}, err
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode config: multiple JSON values")
		}
		return fmt.Errorf("decode config: %w", err)
	}
	return nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "127.0.0.1:8080"
	}
	if cfg.Server.AuthTokenEnv == "" {
		cfg.Server.AuthTokenEnv = "HEALTHZ_API_TOKEN"
	}
	if cfg.Alerts.FailureThreshold == 0 {
		cfg.Alerts.FailureThreshold = 2
	}
	if cfg.SMTP.Port == 0 {
		cfg.SMTP.Port = 587
	}
	if cfg.SMTP.TLS == "" {
		cfg.SMTP.TLS = "starttls"
	}
	if cfg.Digest.Enabled {
		if cfg.Digest.SendAt == "" {
			cfg.Digest.SendAt = "08:00"
		}
		if cfg.Digest.Timezone == "" {
			cfg.Digest.Timezone = "UTC"
		}
	}
	if cfg.Storage.Path == "" {
		cfg.Storage.Path = "data/healthz.db"
	}
	if cfg.Storage.HistoryRetentionDays == 0 {
		cfg.Storage.HistoryRetentionDays = 30
	}
	for i := range cfg.Endpoints {
		endpoint := &cfg.Endpoints[i]
		endpoint.Type = strings.ToLower(endpoint.Type)
		if endpoint.Type == "" {
			endpoint.Type = EndpointTypeHTTP
		}
		if endpoint.Type == EndpointTypeHTTP && endpoint.Method == "" {
			endpoint.Method = "GET"
		}
		endpoint.Method = strings.ToUpper(endpoint.Method)
		if endpoint.Type == EndpointTypePostgres && endpoint.URL == "" {
			endpoint.URL = PostgresTarget(endpoint.DSNEnv)
		}
		if endpoint.Type == EndpointTypePing {
			if endpoint.URL == "" {
				endpoint.URL = PingTarget(endpoint.PingKeyEnv)
			}
			// A ping endpoint only reads local state, so it can be evaluated often,
			// and a single failed evaluation already reflects a missed or failed run.
			if endpoint.Frequency.Duration == 0 {
				endpoint.Frequency.Duration = time.Minute
			}
			if endpoint.FailureThreshold == 0 {
				endpoint.FailureThreshold = 1
			}
			if endpoint.Grace.Duration == 0 {
				endpoint.Grace.Duration = time.Hour
			}
			if endpoint.Schedule != "" && endpoint.Timezone == "" {
				endpoint.Timezone = "UTC"
			}
		}
		if endpoint.Timeout.Duration == 0 {
			endpoint.Timeout.Duration = 10 * time.Second
		}
		if endpoint.Flapping.Enabled {
			if endpoint.Flapping.Transitions == 0 {
				endpoint.Flapping.Transitions = 4
			}
			if endpoint.Flapping.Window.Duration == 0 {
				endpoint.Flapping.Window.Duration = 10 * time.Minute
			}
		}
	}
}

func (cfg Config) Validate() error {
	if strings.TrimSpace(cfg.Server.AuthTokenEnv) == "" {
		return errors.New("config: server.auth_token_env is required")
	}
	if len(cfg.Endpoints) == 0 {
		return errors.New("config: at least one endpoint is required")
	}
	if cfg.Alerts.FailureThreshold < 1 {
		return errors.New("config: alerts.failure_threshold must be at least 1")
	}
	if cfg.Alerts.RepeatInterval.Duration < 0 {
		return errors.New("config: alerts.repeat_interval cannot be negative")
	}
	if strings.TrimSpace(cfg.Storage.Path) == "" {
		return errors.New("config: storage.path is required")
	}
	if cfg.Storage.HistoryRetentionDays < 1 {
		return errors.New("config: storage.history_retention_days must be at least 1")
	}
	if cfg.Digest.Enabled {
		if _, err := time.Parse("15:04", cfg.Digest.SendAt); err != nil {
			return errors.New("config: status_digest.send_at must use 24-hour HH:MM format")
		}
		if _, err := time.LoadLocation(cfg.Digest.Timezone); err != nil {
			return fmt.Errorf("config: invalid status_digest.timezone %q: %w", cfg.Digest.Timezone, err)
		}
		if !cfg.SMTP.Enabled {
			return errors.New("config: status_digest requires SMTP to be enabled")
		}
	}
	if cfg.SMTP.Enabled {
		if cfg.SMTP.Host == "" || cfg.SMTP.From == "" || len(cfg.SMTP.To) == 0 {
			return errors.New("config: enabled SMTP requires host, from, and at least one recipient")
		}
		if cfg.SMTP.Port < 1 || cfg.SMTP.Port > 65535 {
			return errors.New("config: smtp.port must be between 1 and 65535")
		}
		switch cfg.SMTP.TLS {
		case "starttls", "implicit", "none":
		default:
			return errors.New("config: smtp.tls must be starttls, implicit, or none")
		}
		if cfg.SMTP.Username != "" && cfg.SMTP.PasswordEnv == "" {
			return errors.New("config: smtp.password_env is required when username is set")
		}
		if cfg.SMTP.TLS == "none" && cfg.SMTP.Username != "" {
			return errors.New("config: SMTP authentication requires starttls or implicit TLS")
		}
		if !validMailbox(cfg.SMTP.From) {
			return errors.New("config: smtp.from must be a plain email address")
		}
		for _, recipient := range cfg.SMTP.To {
			if !validMailbox(recipient) {
				return fmt.Errorf("config: invalid SMTP recipient %q", recipient)
			}
		}
	}

	names := make(map[string]struct{}, len(cfg.Endpoints))
	for i, endpoint := range cfg.Endpoints {
		prefix := fmt.Sprintf("config: endpoints[%d]", i)
		if strings.TrimSpace(endpoint.Name) == "" {
			return fmt.Errorf("%s.name is required", prefix)
		}
		if strings.ContainsAny(endpoint.Name, "\r\n") {
			return fmt.Errorf("%s.name cannot contain a newline", prefix)
		}
		if _, exists := names[endpoint.Name]; exists {
			return fmt.Errorf("%s.name %q is duplicated", prefix, endpoint.Name)
		}
		names[endpoint.Name] = struct{}{}
		switch endpoint.Type {
		case EndpointTypeHTTP:
			if err := validateHTTPEndpoint(endpoint, prefix); err != nil {
				return err
			}
		case EndpointTypePostgres:
			if err := validatePostgresEndpoint(endpoint, prefix); err != nil {
				return err
			}
		case EndpointTypePing:
			if err := validatePingEndpoint(endpoint, prefix); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s.type must be http, postgres, or ping", prefix)
		}
		if endpoint.Type != EndpointTypePing && hasPingFields(endpoint) {
			return fmt.Errorf("%s.ping_key_env, schedule, timezone, period, and grace require type ping", prefix)
		}
		if endpoint.Frequency.Duration <= 0 {
			return fmt.Errorf("%s.frequency must be positive", prefix)
		}
		if endpoint.Timeout.Duration <= 0 {
			return fmt.Errorf("%s.timeout must be positive", prefix)
		}
		if endpoint.ScheduleJitter.Duration < 0 {
			return fmt.Errorf("%s.schedule_jitter cannot be negative", prefix)
		}
		if endpoint.ScheduleJitter.Duration > endpoint.Frequency.Duration {
			return fmt.Errorf("%s.schedule_jitter cannot exceed frequency", prefix)
		}
		if endpoint.ScheduleJitter.Duration > 0 && endpoint.Frequency.Duration > time.Duration(1<<63-1)-endpoint.ScheduleJitter.Duration {
			return fmt.Errorf("%s.frequency plus schedule_jitter is too large", prefix)
		}
		if endpoint.Flapping.Transitions < 0 {
			return fmt.Errorf("%s.flapping.transitions cannot be negative", prefix)
		}
		if endpoint.Flapping.Window.Duration < 0 {
			return fmt.Errorf("%s.flapping.window cannot be negative", prefix)
		}
		if endpoint.Flapping.Enabled {
			if endpoint.Flapping.Transitions < 2 {
				return fmt.Errorf("%s.flapping.transitions must be at least 2", prefix)
			}
			if endpoint.Flapping.Window.Duration <= 0 {
				return fmt.Errorf("%s.flapping.window must be positive", prefix)
			}
		}
		for field, subject := range map[string]struct {
			value        string
			placeholders []string
		}{
			"down":      {endpoint.AlertSubjects.Down, []string{"name", "failures"}},
			"recovered": {endpoint.AlertSubjects.Recovered, []string{"name", "status"}},
			"flapping":  {endpoint.AlertSubjects.Flapping, []string{"name", "transitions", "window"}},
		} {
			if err := validateSubjectTemplate(subject.value, subject.placeholders); err != nil {
				return fmt.Errorf("%s.alert_subjects.%s %w", prefix, field, err)
			}
		}
		if endpoint.FailureThreshold < 0 {
			return fmt.Errorf("%s.failure_threshold cannot be negative", prefix)
		}
		seenRecipients := make(map[string]struct{}, len(endpoint.AlertTo))
		for _, recipient := range endpoint.AlertTo {
			if !validMailbox(recipient) {
				return fmt.Errorf("%s.alert_to contains invalid recipient %q", prefix, recipient)
			}
			if _, exists := seenRecipients[recipient]; exists {
				return fmt.Errorf("%s.alert_to contains duplicate recipient %q", prefix, recipient)
			}
			seenRecipients[recipient] = struct{}{}
		}
	}
	return nil
}

func validateHTTPEndpoint(endpoint EndpointConfig, prefix string) error {
	parsedURL, err := url.ParseRequestURI(endpoint.URL)
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return fmt.Errorf("%s.url must begin with http:// or https://", prefix)
	}
	if parsedURL.User != nil {
		return fmt.Errorf("%s.url cannot contain credentials; use headers instead", prefix)
	}
	if endpoint.Method != "GET" && endpoint.Method != "HEAD" {
		return fmt.Errorf("%s.method must be GET or HEAD", prefix)
	}
	if endpoint.Method == "HEAD" && endpoint.BodyContains != "" {
		return fmt.Errorf("%s.body_contains cannot be used with HEAD", prefix)
	}
	if endpoint.DSNEnv != "" || endpoint.Query != "" {
		return fmt.Errorf("%s.dsn_env and query require type postgres", prefix)
	}
	for key, value := range endpoint.Headers {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n:") {
			return fmt.Errorf("%s.headers contains invalid name %q", prefix, key)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s.headers[%q] contains a newline", prefix, key)
		}
	}
	for _, status := range endpoint.ExpectedStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("%s.expected_statuses contains invalid HTTP status %d", prefix, status)
		}
	}
	return nil
}

func validatePostgresEndpoint(endpoint EndpointConfig, prefix string) error {
	// The DSN carries credentials, so it is only ever read from the environment.
	if !envName.MatchString(endpoint.DSNEnv) {
		return fmt.Errorf("%s.dsn_env must name an environment variable", prefix)
	}
	if endpoint.URL != PostgresTarget(endpoint.DSNEnv) {
		return fmt.Errorf("%s.url is not used by postgres checks; set dsn_env instead", prefix)
	}
	if strings.TrimSpace(endpoint.Query) == "" {
		return fmt.Errorf("%s.query is required for postgres checks", prefix)
	}
	if endpoint.Method != "" || endpoint.BodyContains != "" || len(endpoint.Headers) > 0 || len(endpoint.ExpectedStatuses) > 0 {
		return fmt.Errorf("%s.method, body_contains, headers, and expected_statuses require type http", prefix)
	}
	return nil
}

func validatePingEndpoint(endpoint EndpointConfig, prefix string) error {
	// The key is the credential for reporting, so it is only read from the environment.
	if !envName.MatchString(endpoint.PingKeyEnv) {
		return fmt.Errorf("%s.ping_key_env must name an environment variable", prefix)
	}
	if endpoint.URL != PingTarget(endpoint.PingKeyEnv) {
		return fmt.Errorf("%s.url is not used by ping checks; set ping_key_env instead", prefix)
	}
	if endpoint.Method != "" || endpoint.BodyContains != "" || len(endpoint.Headers) > 0 || len(endpoint.ExpectedStatuses) > 0 {
		return fmt.Errorf("%s.method, body_contains, headers, and expected_statuses require type http", prefix)
	}
	if endpoint.DSNEnv != "" || endpoint.Query != "" {
		return fmt.Errorf("%s.dsn_env and query require type postgres", prefix)
	}
	if endpoint.RecheckBeforeAlert {
		return fmt.Errorf("%s.recheck_before_alert is not supported by ping checks", prefix)
	}
	if (endpoint.Schedule == "") == (endpoint.Period.Duration == 0) {
		return fmt.Errorf("%s requires exactly one of schedule or period", prefix)
	}
	if endpoint.Period.Duration < 0 {
		return fmt.Errorf("%s.period must be positive", prefix)
	}
	if endpoint.Period.Duration > 0 && endpoint.Timezone != "" {
		return fmt.Errorf("%s.timezone requires schedule", prefix)
	}
	if endpoint.Grace.Duration <= 0 {
		return fmt.Errorf("%s.grace must be positive", prefix)
	}
	schedule, err := endpoint.PingSchedule()
	if err != nil {
		return fmt.Errorf("%s.%w", prefix, err)
	}
	if schedule.Next(time.Now()).IsZero() {
		return fmt.Errorf("%s.schedule never matches a time", prefix)
	}
	return nil
}

func hasPingFields(endpoint EndpointConfig) bool {
	return endpoint.PingKeyEnv != "" || endpoint.Schedule != "" || endpoint.Timezone != "" ||
		endpoint.Period.Duration != 0 || endpoint.Grace.Duration != 0
}

// PingSchedule reports when a ping endpoint next expects a ping.
type PingSchedule interface {
	// Next returns the first expected ping time after t, or the zero time when
	// none is expected.
	Next(t time.Time) time.Time
}

type periodSchedule time.Duration

func (p periodSchedule) Next(t time.Time) time.Time {
	return t.Add(time.Duration(p))
}

// PingSchedule parses a ping endpoint's schedule or period.
func (endpoint EndpointConfig) PingSchedule() (PingSchedule, error) {
	if endpoint.Schedule == "" {
		if endpoint.Period.Duration <= 0 {
			return nil, errors.New("period must be positive")
		}
		return periodSchedule(endpoint.Period.Duration), nil
	}
	expression := strings.TrimSpace(endpoint.Schedule)
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		return nil, errors.New("schedule cannot set a time zone; use timezone instead")
	}
	location, err := time.LoadLocation(endpoint.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q is invalid: %w", endpoint.Timezone, err)
	}
	parsed, err := cron.ParseStandard(expression)
	if err != nil {
		return nil, fmt.Errorf("schedule %q is not a valid cron expression: %w", endpoint.Schedule, err)
	}
	spec, ok := parsed.(*cron.SpecSchedule)
	if !ok {
		return nil, errors.New("schedule cannot use @every; use period instead")
	}
	spec.Location = location
	return spec, nil
}

// PingTarget names a ping check's target without revealing its key.
func PingTarget(keyEnv string) string {
	return "ping:$" + keyEnv
}

// PostgresTarget names a postgres check's target without revealing its DSN.
func PostgresTarget(dsnEnv string) string {
	return "postgres:$" + dsnEnv
}

// RequiredEnv lists environment variables that endpoint checks read at runtime.
func (cfg Config) RequiredEnv() []string {
	var names []string
	seen := make(map[string]struct{})
	for _, endpoint := range cfg.Endpoints {
		var name string
		switch endpoint.Type {
		case EndpointTypePostgres:
			name = endpoint.DSNEnv
		case EndpointTypePing:
			name = endpoint.PingKeyEnv
		default:
			continue
		}
		if _, exists := seen[name]; !exists {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}

func validMailbox(value string) bool {
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func validateSubjectTemplate(value string, allowedPlaceholders []string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("cannot contain a newline")
	}
	if len(value) > 200 {
		return errors.New("cannot exceed 200 bytes")
	}
	allowed := make(map[string]struct{}, len(allowedPlaceholders))
	for _, placeholder := range allowedPlaceholders {
		allowed[placeholder] = struct{}{}
	}
	remaining := value
	for len(remaining) > 0 {
		open := strings.IndexByte(remaining, '{')
		close := strings.IndexByte(remaining, '}')
		if close >= 0 && (open < 0 || close < open) {
			return errors.New("contains an unmatched closing brace")
		}
		if open < 0 {
			return nil
		}
		close = strings.IndexByte(remaining[open+1:], '}')
		if close < 0 {
			return errors.New("contains an unmatched opening brace")
		}
		close += open + 1
		placeholder := remaining[open+1 : close]
		if _, ok := allowed[placeholder]; !ok {
			return fmt.Errorf("contains unsupported placeholder %q", "{"+placeholder+"}")
		}
		remaining = remaining[close+1:]
	}
	return nil
}

func (cfg Config) ThresholdFor(endpoint EndpointConfig) int {
	if endpoint.FailureThreshold > 0 {
		return endpoint.FailureThreshold
	}
	return cfg.Alerts.FailureThreshold
}
