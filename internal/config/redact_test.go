package config

import "testing"

func TestRedactURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://example.com/health":                        "https://example.com/health",
		"https://example.com/health?token=s3cret&region=eu": "https://example.com/health?token=REDACTED&region=REDACTED",
		"https://example.com/health?s3cret":                 "https://example.com/health?REDACTED",
		"https://example.com/health?a=1&&b":                 "https://example.com/health?a=REDACTED&&REDACTED",
		"https://user:pass@example.com/":                    "https://REDACTED@example.com/",
		"https://example.com/health#frag":                   "https://example.com/health",
		"http://[::1]:8080/x?key=s3cret":                    "http://[::1]:8080/x?key=REDACTED",
		"https://example.com/%zz?token=s3cret":              "https://example.com/%zz?REDACTED",
		PostgresTarget("APP_DSN"):                           PostgresTarget("APP_DSN"),
		PingTarget("BACKUP_PING_KEY"):                       PingTarget("BACKUP_PING_KEY"),
	} {
		if got := RedactURL(raw); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
