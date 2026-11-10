package alert

import (
	"strings"
	"testing"

	"github.com/runeQ/healthz/internal/config"
)

func TestBuildMessagePreventsHeaderInjection(t *testing.T) {
	payload := buildMessage(
		config.SMTPConfig{From: "monitor@example.com\r\nBcc: attacker@example.com"},
		[]string{"alerts@example.com\nBcc: attacker@example.com"},
		Message{Subject: "down\r\nBcc: attacker@example.com", Body: "line one\nline two"},
	)

	if strings.Contains(payload, "\r\nBcc:") {
		t.Fatalf("header injection was not flattened: %q", payload)
	}
	if !strings.Contains(payload, "line one\r\nline two") {
		t.Fatalf("body does not use CRLF: %q", payload)
	}
}

func TestMessageRecipientsUseEndpointOverride(t *testing.T) {
	cfg := config.SMTPConfig{To: []string{"global@example.com"}}
	override := []string{"team@example.com", "operations@example.com"}

	got := messageRecipients(cfg, Message{Recipients: override})
	if strings.Join(got, ",") != strings.Join(override, ",") {
		t.Fatalf("recipients = %v, want endpoint override %v", got, override)
	}
	payload := buildMessage(cfg, got, Message{Subject: "down", Body: "failed"})
	if !strings.Contains(payload, "To: team@example.com, operations@example.com\r\n") {
		t.Fatalf("override recipients missing from message: %q", payload)
	}
	if strings.Contains(payload, "global@example.com") {
		t.Fatalf("global recipient used despite override: %q", payload)
	}
}

func TestMessageRecipientsFallBackToGlobal(t *testing.T) {
	cfg := config.SMTPConfig{To: []string{"global@example.com", "owner@example.com"}}
	got := messageRecipients(cfg, Message{})
	if strings.Join(got, ",") != strings.Join(cfg.To, ",") {
		t.Fatalf("recipients = %v, want global recipients %v", got, cfg.To)
	}
}
