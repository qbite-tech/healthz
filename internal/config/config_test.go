package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeConfig(t, `{
  "endpoints": [{
    "name": "site",
    "url": "https://example.com",
    "frequency": "30s"
  }]
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q, want 127.0.0.1:8080", cfg.Server.Listen)
	}
	if cfg.Server.AuthTokenEnv != "HEALTHZ_API_TOKEN" {
		t.Errorf("auth token environment variable = %q, want HEALTHZ_API_TOKEN", cfg.Server.AuthTokenEnv)
	}
	if cfg.Alerts.FailureThreshold != 2 {
		t.Errorf("failure threshold = %d, want 2", cfg.Alerts.FailureThreshold)
	}
	if got := cfg.Endpoints[0].Timeout.Duration; got != 10*time.Second {
		t.Errorf("timeout = %s, want 10s", got)
	}
	if cfg.Endpoints[0].Method != "GET" {
		t.Errorf("method = %q, want GET", cfg.Endpoints[0].Method)
	}
	if cfg.Storage.Path != "data/healthz.db" || cfg.Storage.HistoryRetentionDays != 30 {
		t.Errorf("storage defaults = %+v, want data/healthz.db and 30 days", cfg.Storage)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{
  "unknown": true,
  "endpoints": [{"name":"site","url":"https://example.com","frequency":"30s"}]
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v, want unknown field error", err)
	}
}

func TestLoadRejectsDuplicateEndpointNames(t *testing.T) {
	path := writeConfig(t, `{
  "endpoints": [
    {"name":"site","url":"https://example.com","frequency":"30s"},
    {"name":"site","url":"https://example.org","frequency":"1m"}
  ]
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("Load() error = %v, want duplicate error", err)
	}
}

func TestDurationRequiresReadableString(t *testing.T) {
	path := writeConfig(t, `{
  "endpoints": [{"name":"site","url":"https://example.com","frequency":30}]
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "duration must be a string") {
		t.Fatalf("Load() error = %v, want duration string error", err)
	}
}

func TestDigestDefaults(t *testing.T) {
	path := writeConfig(t, `{
  "smtp": {
    "enabled": true,
    "host": "smtp.example.com",
    "from": "monitor@example.com",
    "to": ["alerts@example.com"]
  },
  "status_digest": {"enabled": true},
  "endpoints": [{"name":"site","url":"https://example.com","frequency":"30s"}]
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Digest.SendAt != "08:00" || cfg.Digest.Timezone != "UTC" {
		t.Fatalf("digest defaults = %+v, want 08:00 UTC", cfg.Digest)
	}
}

func TestDigestRequiresValidScheduleAndSMTP(t *testing.T) {
	path := writeConfig(t, `{
  "status_digest": {
    "enabled": true,
    "send_at": "8am",
    "timezone": "Nowhere/Imaginary"
  },
  "endpoints": [{"name":"site","url":"https://example.com","frequency":"30s"}]
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "HH:MM") {
		t.Fatalf("Load() error = %v, want invalid digest time error", err)
	}
}

func TestStorageRequiresPositiveRetention(t *testing.T) {
	path := writeConfig(t, `{
  "storage": {"path": "data/test.db", "history_retention_days": -1},
  "endpoints": [{"name":"site","url":"https://example.com","frequency":"30s"}]
}`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "history_retention_days") {
		t.Fatalf("Load() error = %v, want retention error", err)
	}
}

func TestEndpointAlertRecipients(t *testing.T) {
	path := writeConfig(t, `{
  "endpoints": [{
    "name":"site",
    "url":"https://example.com",
    "frequency":"30s",
    "alert_to":["team@example.com", "operations@example.com"]
  }]
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Endpoints[0].AlertTo; len(got) != 2 || got[0] != "team@example.com" || got[1] != "operations@example.com" {
		t.Fatalf("alert_to = %v", got)
	}
}

func TestEndpointAlertRecipientsMustBeValidAndUnique(t *testing.T) {
	for name, recipients := range map[string]string{
		"invalid":   `"not-an-email"`,
		"duplicate": `"team@example.com", "team@example.com"`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
  "endpoints": [{
    "name":"site",
    "url":"https://example.com",
    "frequency":"30s",
    "alert_to":[`+recipients+`]
  }]
}`)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "alert_to") {
				t.Fatalf("Load() error = %v, want alert_to validation error", err)
			}
		})
	}
}

func TestImportantEndpointDefaults(t *testing.T) {
	path := writeConfig(t, `{
  "endpoints": [{
    "name":"site",
    "url":"https://example.com",
    "frequency":"30s",
    "schedule_jitter":"5s",
    "recheck_before_alert":true,
    "flapping":{"enabled":true}
  }]
}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := cfg.Endpoints[0]
	if !endpoint.RecheckBeforeAlert || endpoint.ScheduleJitter.Duration != 5*time.Second {
		t.Fatalf("important endpoint options = %+v", endpoint)
	}
	if endpoint.Flapping.Transitions != 4 || endpoint.Flapping.Window.Duration != 10*time.Minute {
		t.Fatalf("flapping defaults = %+v, want 4 transitions in 10m", endpoint.Flapping)
	}
}

func TestImportantEndpointOptionsAreValidated(t *testing.T) {
	for name, options := range map[string]string{
		"excessive jitter": `"schedule_jitter":"31s"`,
		"few transitions":  `"flapping":{"enabled":true,"transitions":1,"window":"10m"}`,
		"negative window":  `"flapping":{"enabled":false,"transitions":4,"window":"-1s"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
  "endpoints": [{
    "name":"site",
    "url":"https://example.com",
    "frequency":"30s",
    `+options+`
  }]
}`)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() succeeded, want validation error")
			}
		})
	}
}

func TestCustomAlertSubjects(t *testing.T) {
	path := writeConfig(t, `{
  "endpoints": [{
    "name":"site",
    "url":"https://example.com",
    "frequency":"30s",
    "alert_subjects":{
      "down":"[CRITICAL] {name}: {failures} failures",
      "recovered":"[RESOLVED] {name}: HTTP {status}",
      "flapping":"[UNSTABLE] {name}: {transitions} in {window}"
    }
  }]
}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoints[0].AlertSubjects.Down != "[CRITICAL] {name}: {failures} failures" {
		t.Fatalf("alert subjects = %+v", cfg.Endpoints[0].AlertSubjects)
	}
}

func TestCustomAlertSubjectsAreValidated(t *testing.T) {
	for name, subjects := range map[string]string{
		"unknown placeholder": `"down":"{endpoint} is down"`,
		"wrong event":         `"recovered":"{failures} failures"`,
		"header injection":    `"flapping":"unstable\nBcc: attacker@example.com"`,
		"unmatched brace":     `"down":"{name is down"`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
  "endpoints": [{
    "name":"site",
    "url":"https://example.com",
    "frequency":"30s",
    "alert_subjects":{`+subjects+`}
  }]
}`)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "alert_subjects") {
				t.Fatalf("Load() error = %v, want alert_subjects validation error", err)
			}
		})
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPostgresEndpointDefaults(t *testing.T) {
	path := writeConfig(t, `{
  "endpoints": [
    {"name":"site","url":"https://example.com","frequency":"30s"},
    {"name":"workers","type":"Postgres","dsn_env":"MONITOR_DSN","query":"SELECT true","frequency":"1m"},
    {"name":"discovery","type":"postgres","dsn_env":"MONITOR_DSN","query":"SELECT true","frequency":"5m"}
  ]
}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Endpoints[0].Type; got != EndpointTypeHTTP {
		t.Errorf("default type = %q, want http", got)
	}
	workers := cfg.Endpoints[1]
	if workers.Type != EndpointTypePostgres || workers.URL != "postgres:$MONITOR_DSN" || workers.Method != "" {
		t.Errorf("postgres endpoint = type %q, url %q, method %q", workers.Type, workers.URL, workers.Method)
	}
	if got := cfg.RequiredEnv(); len(got) != 1 || got[0] != "MONITOR_DSN" {
		t.Errorf("RequiredEnv() = %v, want [MONITOR_DSN]", got)
	}
}

func TestEndpointTypeOptionsAreValidated(t *testing.T) {
	for name, endpoint := range map[string]string{
		"unknown type":         `"type":"ftp","url":"https://example.com"`,
		"missing dsn_env":      `"type":"postgres","query":"SELECT true"`,
		"invalid dsn_env":      `"type":"postgres","dsn_env":"postgres://user:secret@db/app","query":"SELECT true"`,
		"postgres url":         `"type":"postgres","url":"https://example.com","dsn_env":"DSN","query":"SELECT true"`,
		"missing query":        `"type":"postgres","dsn_env":"DSN","query":"  "`,
		"postgres http option": `"type":"postgres","dsn_env":"DSN","query":"SELECT true","expected_statuses":[200]`,
		"postgres method":      `"type":"postgres","dsn_env":"DSN","query":"SELECT true","method":"GET"`,
		"http with query":      `"url":"https://example.com","query":"SELECT true"`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
  "endpoints": [{"name":"check","frequency":"30s",`+endpoint+`}]
}`)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() succeeded, want validation error")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation error reveals a DSN: %v", err)
			}
		})
	}
}

func TestPingEndpointDefaults(t *testing.T) {
	path := writeConfig(t, `{
  "alerts": {"failure_threshold": 3},
  "endpoints": [
    {"name":"backup","type":"ping","ping_key_env":"BACKUP_PING_KEY","schedule":"30 2 * * *"},
    {"name":"sync","type":"ping","ping_key_env":"SYNC_PING_KEY","period":"15m","grace":"5m","frequency":"30s","failure_threshold":2}
  ]
}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	backup := cfg.Endpoints[0]
	if backup.URL != "ping:$BACKUP_PING_KEY" || backup.Method != "" || backup.Timezone != "UTC" ||
		backup.Frequency.Duration != time.Minute || backup.Grace.Duration != time.Hour || cfg.ThresholdFor(backup) != 1 {
		t.Errorf("ping defaults = url %q, method %q, timezone %q, frequency %s, grace %s, threshold %d",
			backup.URL, backup.Method, backup.Timezone, backup.Frequency.Duration, backup.Grace.Duration, cfg.ThresholdFor(backup))
	}
	sync := cfg.Endpoints[1]
	if sync.Timezone != "" || sync.Frequency.Duration != 30*time.Second || sync.Grace.Duration != 5*time.Minute || cfg.ThresholdFor(sync) != 2 {
		t.Errorf("ping overrides were not kept: %+v", sync)
	}
	if got := cfg.RequiredEnv(); len(got) != 2 || got[0] != "BACKUP_PING_KEY" || got[1] != "SYNC_PING_KEY" {
		t.Errorf("RequiredEnv() = %v", got)
	}
}

func TestPingScheduleUsesTimezone(t *testing.T) {
	endpoint := EndpointConfig{Schedule: "30 2 * * *", Timezone: "Europe/Copenhagen"}
	schedule, err := endpoint.PingSchedule()
	if err != nil {
		t.Fatal(err)
	}
	// Copenhagen is UTC+2 in summer and UTC+1 after the October DST change.
	for after, want := range map[string]string{
		"2026-07-01T12:00:00Z": "2026-07-02T00:30:00Z",
		"2026-11-01T12:00:00Z": "2026-11-02T01:30:00Z",
	} {
		from, _ := time.Parse(time.RFC3339, after)
		if got := schedule.Next(from).UTC().Format(time.RFC3339); got != want {
			t.Errorf("Next(%s) = %s, want %s", after, got, want)
		}
	}
}

func TestPingEndpointOptionsAreValidated(t *testing.T) {
	for name, endpoint := range map[string]string{
		"missing key env":        `"type":"ping","schedule":"30 2 * * *"`,
		"literal key":            `"type":"ping","ping_key_env":"https://hc-ping.com/abc","schedule":"30 2 * * *"`,
		"ping url":               `"type":"ping","ping_key_env":"KEY","url":"https://example.com","schedule":"30 2 * * *"`,
		"no schedule or period":  `"type":"ping","ping_key_env":"KEY"`,
		"schedule and period":    `"type":"ping","ping_key_env":"KEY","schedule":"30 2 * * *","period":"1h"`,
		"negative period":        `"type":"ping","ping_key_env":"KEY","period":"-1h"`,
		"negative grace":         `"type":"ping","ping_key_env":"KEY","period":"1h","grace":"-1m"`,
		"invalid cron":           `"type":"ping","ping_key_env":"KEY","schedule":"61 2 * * *"`,
		"cron with seconds":      `"type":"ping","ping_key_env":"KEY","schedule":"0 30 2 * * *"`,
		"cron every":             `"type":"ping","ping_key_env":"KEY","schedule":"@every 1h"`,
		"cron time zone prefix":  `"type":"ping","ping_key_env":"KEY","schedule":"CRON_TZ=UTC 30 2 * * *"`,
		"never matches":          `"type":"ping","ping_key_env":"KEY","schedule":"0 0 30 2 *"`,
		"invalid timezone":       `"type":"ping","ping_key_env":"KEY","schedule":"30 2 * * *","timezone":"Mars/Base"`,
		"timezone with period":   `"type":"ping","ping_key_env":"KEY","period":"1h","timezone":"UTC"`,
		"ping http option":       `"type":"ping","ping_key_env":"KEY","period":"1h","expected_statuses":[200]`,
		"ping postgres option":   `"type":"ping","ping_key_env":"KEY","period":"1h","query":"SELECT true"`,
		"ping recheck":           `"type":"ping","ping_key_env":"KEY","period":"1h","recheck_before_alert":true`,
		"http with ping option":  `"url":"https://example.com","frequency":"30s","grace":"1h"`,
		"postgres with schedule": `"type":"postgres","dsn_env":"DSN","query":"SELECT true","frequency":"30s","schedule":"30 2 * * *"`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, `{
  "endpoints": [{"name":"check",`+endpoint+`}]
}`)
			if _, err := Load(path); err == nil {
				t.Fatal("Load() succeeded, want validation error")
			}
		})
	}
}
