package monitor

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runeQ/healthz/internal/config"
)

func TestAPITokenLifecycle(t *testing.T) {
	store := NewStatusStore([]config.EndpointConfig{{Name: "site", URL: "https://example.com"}})
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Round(0)
	expires := now.Add(24 * time.Hour)

	created, err := store.CreateAPIToken(ctx, "Rune laptop", "rune@example.com", &expires, now)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || !strings.HasPrefix(created.Plaintext, "hz_") || created.Name != "Rune laptop" {
		t.Fatalf("created token = %+v", created)
	}
	if id, ok := parseAPIToken(created.Plaintext); !ok || id != created.ID {
		t.Fatalf("could not parse created token %q", created.Plaintext)
	}

	var storedHash []byte
	if err := store.db.QueryRow("SELECT token_hash FROM api_tokens WHERE id = ?", created.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256([]byte(created.Plaintext))
	if string(storedHash) != string(expectedHash[:]) || string(storedHash) == created.Plaintext {
		t.Fatal("database did not retain only the expected token hash")
	}

	usable, err := store.HasUsableAPITokens(ctx, now)
	if err != nil || !usable {
		t.Fatalf("HasUsableAPITokens() = %v, %v", usable, err)
	}
	identity, err := store.AuthenticateAPIToken(ctx, created.Plaintext, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != created.ID || identity.Name != created.Name || identity.Email != created.Email {
		t.Fatalf("authenticated identity = %+v", identity)
	}

	tokens, err := store.ListAPITokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].LastUsedAt == nil || !tokens[0].LastUsedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("listed tokens = %+v", tokens)
	}

	wrong := created.Plaintext[:len(created.Plaintext)-1] + differentBase64Character(created.Plaintext[len(created.Plaintext)-1])
	if _, err := store.AuthenticateAPIToken(ctx, wrong, now); !errors.Is(err, ErrInvalidAPIToken) {
		t.Fatalf("wrong token error = %v", err)
	}
	if _, err := store.AuthenticateAPIToken(ctx, created.Plaintext, expires); !errors.Is(err, ErrInvalidAPIToken) {
		t.Fatalf("expired token error = %v", err)
	}
	if err := store.RevokeAPIToken(ctx, created.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateAPIToken(ctx, created.Plaintext, now.Add(2*time.Hour)); !errors.Is(err, ErrInvalidAPIToken) {
		t.Fatalf("revoked token error = %v", err)
	}
	if err := store.RevokeAPIToken(ctx, created.ID, now); !errors.Is(err, ErrAPITokenNotFound) {
		t.Fatalf("second revoke error = %v", err)
	}
}

func TestCreateAPITokenValidatesMetadataAndExpiry(t *testing.T) {
	store := NewStatusStore([]config.EndpointConfig{{Name: "site", URL: "https://example.com"}})
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	for name, input := range map[string]struct {
		name      string
		email     string
		expiresAt *time.Time
	}{
		"missing name":  {email: "rune@example.com"},
		"newline name":  {name: "Rune\nadmin"},
		"tab name":      {name: "Rune\tadmin"},
		"display email": {name: "Rune", email: "Rune <rune@example.com>"},
		"expired":       {name: "Rune", expiresAt: timePointer(now.Add(-time.Second))},
		"far future":    {name: "Rune", expiresAt: timePointer(time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.CreateAPIToken(context.Background(), input.name, input.email, input.expiresAt, now); err == nil {
				t.Fatal("CreateAPIToken() succeeded")
			}
		})
	}
}

func TestAPITokenChangesAreVisibleWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthz.db")
	endpoint := config.EndpointConfig{Name: "site", URL: "https://example.com"}
	serviceStore := openTestStore(t, path, endpoint)
	t.Cleanup(func() { _ = serviceStore.Close() })
	adminStore, err := OpenTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminStore.Close() })
	now := time.Now()

	created, err := adminStore.CreateAPIToken(context.Background(), "live rotation", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceStore.AuthenticateAPIToken(context.Background(), created.Plaintext, now); err != nil {
		t.Fatalf("running service did not see issued token: %v", err)
	}
	if err := adminStore.RevokeAPIToken(context.Background(), created.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceStore.AuthenticateAPIToken(context.Background(), created.Plaintext, now.Add(2*time.Second)); !errors.Is(err, ErrInvalidAPIToken) {
		t.Fatalf("running service did not see token revocation: %v", err)
	}
	var enabled int
	if err := serviceStore.db.QueryRow("SELECT enabled FROM endpoints WHERE name = ?", endpoint.Name).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatalf("token administration changed endpoint enabled state to %d", enabled)
	}
}

func TestHandlerAcceptsDatabaseTokenAndBootstrapToken(t *testing.T) {
	store := NewStatusStore([]config.EndpointConfig{{Name: "site", URL: "https://example.com"}})
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateAPIToken(context.Background(), "automation", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	handler := store.Handler("bootstrap-token")

	for name, token := range map[string]string{
		"database":  created.Plaintext,
		"bootstrap": "bootstrap-token",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/status", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestHandlerRejectsExpiredDatabaseToken(t *testing.T) {
	store := NewStatusStore([]config.EndpointConfig{{Name: "site", URL: "https://example.com"}})
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	expires := now.Add(20 * time.Millisecond)
	created, err := store.CreateAPIToken(context.Background(), "short lived", "", &expires, now)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	request := httptest.NewRequest(http.MethodGet, "/status", nil)
	request.Header.Set("Authorization", "Bearer "+created.Plaintext)
	response := httptest.NewRecorder()
	store.Handler("").ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func differentBase64Character(value byte) string {
	if value == 'A' {
		return "B"
	}
	return "A"
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func TestValidateBootstrapToken(t *testing.T) {
	for token, valid := range map[string]bool{
		"":                                   true,
		"0123456789abcdef0123456789abcdef":   true,
		strings.Repeat("a", 64):              true,
		"short-token":                        false,
		strings.Repeat("a", 31):              false,
		"0123456789abcdef 0123456789abcdef":  false,
		"0123456789abcdef0123456789abcdef\n": false,
	} {
		err := ValidateBootstrapToken(token)
		if (err == nil) != valid {
			t.Errorf("ValidateBootstrapToken(%q) = %v, want valid=%v", token, err, valid)
		}
		if err != nil && token != "" && strings.Contains(err.Error(), token) {
			t.Errorf("error reveals the token: %v", err)
		}
	}
}
