package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode"
)

const (
	apiTokenPrefix       = "hz_"
	apiTokenIDBytes      = 12
	apiTokenSecretBytes  = 32
	apiTokenIDLength     = 16
	apiTokenSecretLength = 43
)

var (
	ErrInvalidAPIToken  = errors.New("invalid API token")
	ErrAPITokenNotFound = errors.New("API token not found or already revoked")
	maxAPITokenExpiry   = time.Unix(0, math.MaxInt64).UTC()
)

// APIToken contains the non-secret metadata retained for a personalized API token.
type APIToken struct {
	ID         string
	Name       string
	Email      string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// CreatedAPIToken is returned only when a token is issued. Plaintext is never persisted.
type CreatedAPIToken struct {
	APIToken
	Plaintext string
}

// AuthenticatedAPIToken identifies the database token that authenticated a request.
type AuthenticatedAPIToken struct {
	ID    string
	Name  string
	Email string
}

func (s *StatusStore) CreateAPIToken(ctx context.Context, name, email string, expiresAt *time.Time, now time.Time) (CreatedAPIToken, error) {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name == "" {
		return CreatedAPIToken{}, errors.New("token name is required")
	}
	if len(name) > 200 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return CreatedAPIToken{}, errors.New("token name must be at most 200 bytes and cannot contain control characters")
	}
	if email != "" {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email || len(email) > 320 {
			return CreatedAPIToken{}, errors.New("token email must be a plain email address")
		}
	}
	if expiresAt != nil {
		expiresUTC := expiresAt.UTC()
		if !expiresUTC.After(now) {
			return CreatedAPIToken{}, errors.New("token expiry must be in the future")
		}
		if expiresUTC.After(maxAPITokenExpiry) {
			return CreatedAPIToken{}, fmt.Errorf("token expiry cannot be later than %s", maxAPITokenExpiry.Format(time.RFC3339))
		}
		expiresAt = &expiresUTC
	}
	now = now.UTC()

	for range 5 {
		id, err := randomBase64URL(apiTokenIDBytes)
		if err != nil {
			return CreatedAPIToken{}, fmt.Errorf("generate token ID: %w", err)
		}
		secret, err := randomBase64URL(apiTokenSecretBytes)
		if err != nil {
			return CreatedAPIToken{}, fmt.Errorf("generate token secret: %w", err)
		}
		plaintext := apiTokenPrefix + id + "_" + secret
		hash := sha256.Sum256([]byte(plaintext))
		var expires any
		if expiresAt != nil {
			expires = expiresAt.UnixNano()
		}
		result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO api_tokens(
			id, name, email, token_hash, created_at_ns, expires_at_ns
		) VALUES(?, ?, NULLIF(?, ''), ?, ?, ?)`, id, name, email, hash[:], now.UnixNano(), expires)
		if err != nil {
			return CreatedAPIToken{}, fmt.Errorf("save API token: %w", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return CreatedAPIToken{}, fmt.Errorf("confirm API token creation: %w", err)
		}
		if inserted == 1 {
			return CreatedAPIToken{
				APIToken:  APIToken{ID: id, Name: name, Email: email, CreatedAt: now, ExpiresAt: expiresAt},
				Plaintext: plaintext,
			}, nil
		}
	}
	return CreatedAPIToken{}, errors.New("could not generate a unique API token ID")
}

func (s *StatusStore) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, COALESCE(email, ''), created_at_ns,
		expires_at_ns, revoked_at_ns, last_used_at_ns
		FROM api_tokens ORDER BY created_at_ns DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list API tokens: %w", err)
	}
	defer rows.Close()

	var tokens []APIToken
	for rows.Next() {
		var token APIToken
		var created int64
		var expires, revoked, lastUsed sql.NullInt64
		if err := rows.Scan(&token.ID, &token.Name, &token.Email, &created, &expires, &revoked, &lastUsed); err != nil {
			return nil, fmt.Errorf("read API token: %w", err)
		}
		token.CreatedAt = time.Unix(0, created).UTC()
		token.ExpiresAt = nullableUnixTime(expires)
		token.RevokedAt = nullableUnixTime(revoked)
		token.LastUsedAt = nullableUnixTime(lastUsed)
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list API tokens: %w", err)
	}
	return tokens, nil
}

func (s *StatusStore) RevokeAPIToken(ctx context.Context, id string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at_ns = ?
		WHERE id = ? AND revoked_at_ns IS NULL`, now.UTC().UnixNano(), strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("revoke API token: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm API token revocation: %w", err)
	}
	if changed != 1 {
		return ErrAPITokenNotFound
	}
	return nil
}

func (s *StatusStore) HasUsableAPITokens(ctx context.Context, now time.Time) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM api_tokens
		WHERE revoked_at_ns IS NULL AND (expires_at_ns IS NULL OR expires_at_ns > ?)
	)`, now.UnixNano()).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check active API tokens: %w", err)
	}
	return exists == 1, nil
}

func (s *StatusStore) AuthenticateAPIToken(ctx context.Context, plaintext string, now time.Time) (AuthenticatedAPIToken, error) {
	id, ok := parseAPIToken(plaintext)
	if !ok {
		return AuthenticatedAPIToken{}, ErrInvalidAPIToken
	}
	var identity AuthenticatedAPIToken
	var storedHash []byte
	var expires, revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, COALESCE(email, ''), token_hash,
		expires_at_ns, revoked_at_ns FROM api_tokens WHERE id = ?`, id).
		Scan(&identity.ID, &identity.Name, &identity.Email, &storedHash, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthenticatedAPIToken{}, ErrInvalidAPIToken
	}
	if err != nil {
		return AuthenticatedAPIToken{}, fmt.Errorf("load API token: %w", err)
	}
	providedHash := sha256.Sum256([]byte(plaintext))
	if len(storedHash) != sha256.Size || subtle.ConstantTimeCompare(providedHash[:], storedHash) != 1 ||
		revoked.Valid || (expires.Valid && expires.Int64 <= now.UnixNano()) {
		return AuthenticatedAPIToken{}, ErrInvalidAPIToken
	}
	usedAt := now.UTC().UnixNano()
	result, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at_ns = ?
		WHERE id = ? AND token_hash = ? AND revoked_at_ns IS NULL
		AND (expires_at_ns IS NULL OR expires_at_ns > ?)`, usedAt, id, storedHash, usedAt)
	if err != nil {
		return AuthenticatedAPIToken{}, fmt.Errorf("record API token use: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return AuthenticatedAPIToken{}, fmt.Errorf("confirm API token use: %w", err)
	}
	if changed != 1 {
		return AuthenticatedAPIToken{}, ErrInvalidAPIToken
	}
	return identity, nil
}

func (s *StatusStore) requireAPIToken(bootstrapToken string, logger *slog.Logger, next http.Handler) http.Handler {
	bootstrapHash := sha256.Sum256([]byte(bootstrapToken))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		parts := strings.Fields(request.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeUnauthorized(writer)
			return
		}
		plaintext := parts[1]
		providedHash := sha256.Sum256([]byte(plaintext))
		if bootstrapToken != "" && subtle.ConstantTimeCompare(providedHash[:], bootstrapHash[:]) == 1 {
			if logger != nil {
				logger.Info("authenticated API request", "token_type", "bootstrap", "method", request.Method,
					"path", request.URL.Path, "remote_address", request.RemoteAddr)
			}
			next.ServeHTTP(writer, request)
			return
		}

		identity, err := s.AuthenticateAPIToken(request.Context(), plaintext, time.Now())
		if errors.Is(err, ErrInvalidAPIToken) {
			writeUnauthorized(writer)
			return
		}
		if err != nil {
			if logger != nil {
				logger.Error("could not authenticate API request", "error", err)
			}
			http.Error(writer, "authentication unavailable", http.StatusInternalServerError)
			return
		}
		if logger != nil {
			logger.Info("authenticated API request", "token_type", "personal", "token_id", identity.ID,
				"token_name", identity.Name, "token_email", identity.Email, "method", request.Method,
				"path", request.URL.Path, "remote_address", request.RemoteAddr)
		}
		next.ServeHTTP(writer, request)
	})
}

// minBootstrapTokenLength matches the minimum ping key length; openssl rand
// -hex 32, as the docs suggest, gives 64 characters.
const minBootstrapTokenLength = 32

// ValidateBootstrapToken rejects a bootstrap token that is too short to resist
// guessing or could never match a Bearer header. An empty token is allowed: it
// disables the bootstrap token. The error never contains the token.
func ValidateBootstrapToken(token string) error {
	if token == "" {
		return nil
	}
	if strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("bootstrap API token cannot contain whitespace or control characters")
	}
	if len(token) < minBootstrapTokenLength {
		return fmt.Errorf("bootstrap API token must be at least %d characters; generate one with `openssl rand -hex 32`",
			minBootstrapTokenLength)
	}
	return nil
}

func writeUnauthorized(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="healthz"`)
	http.Error(writer, "unauthorized", http.StatusUnauthorized)
}

func randomBase64URL(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func parseAPIToken(plaintext string) (string, bool) {
	if !strings.HasPrefix(plaintext, apiTokenPrefix) {
		return "", false
	}
	value := strings.TrimPrefix(plaintext, apiTokenPrefix)
	if len(value) != apiTokenIDLength+1+apiTokenSecretLength || value[apiTokenIDLength] != '_' {
		return "", false
	}
	id := value[:apiTokenIDLength]
	secret := value[apiTokenIDLength+1:]
	idBytes, idErr := base64.RawURLEncoding.DecodeString(id)
	secretBytes, secretErr := base64.RawURLEncoding.DecodeString(secret)
	if idErr != nil || secretErr != nil || len(idBytes) != apiTokenIDBytes || len(secretBytes) != apiTokenSecretBytes {
		return "", false
	}
	return id, true
}

func nullableUnixTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.Unix(0, value.Int64).UTC()
	return &result
}
