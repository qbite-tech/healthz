package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenCLIManagesPersistentTokens(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	databasePath := filepath.Join(directory, "healthz.db")
	configJSON := `{
  "storage":{"path":` + quoted(databasePath) + `,"history_retention_days":30},
  "endpoints":[{"name":"site","url":"https://example.com","frequency":"1m"}]
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runTokenCommand([]string{"create", "-config", configPath, "-name", "deployment script",
		"-email", "ops@example.com", "-expires-in", "30d"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("create exit = %d, stderr = %s", code, stderr.String())
	}
	plaintext := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(plaintext, "hz_") || strings.Contains(stderr.String(), plaintext) {
		t.Fatalf("create stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	id := strings.TrimPrefix(plaintext, "hz_")[:apiTokenIDLengthForTest]

	stdout.Reset()
	stderr.Reset()
	code = runTokenCommand([]string{"list", "-config", configPath}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), id) || !strings.Contains(stdout.String(), "ops@example.com") ||
		strings.Contains(stdout.String(), plaintext) {
		t.Fatalf("list exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runTokenCommand([]string{"revoke", "-config", configPath, "-id", id}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "revoked") {
		t.Fatalf("revoke exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestParseTokenLifetime(t *testing.T) {
	for input, expected := range map[string]time.Duration{
		"90m": 90 * time.Minute,
		"30d": 30 * 24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
	} {
		got, err := parseTokenLifetime(input)
		if err != nil || got != expected {
			t.Errorf("parseTokenLifetime(%q) = %s, %v; want %s", input, got, err, expected)
		}
	}
	for _, input := range []string{"", "0h", "-1h", "1.5d", "forever"} {
		if _, err := parseTokenLifetime(input); err == nil {
			t.Errorf("parseTokenLifetime(%q) succeeded", input)
		}
	}
}

func TestResolveTokenExpiryRejectsConflictingOptions(t *testing.T) {
	if _, err := resolveTokenExpiry("24h", "2099-01-01T00:00:00Z", time.Now()); err == nil {
		t.Fatal("resolveTokenExpiry() accepted both expiry forms")
	}
}

const apiTokenIDLengthForTest = 16

func quoted(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
