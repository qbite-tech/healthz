package config

import (
	"net/url"
	"strings"
)

// redacted replaces values that may be secrets in URLs shown to people.
const redacted = "REDACTED"

// RedactURL returns rawURL with every query value and any user information
// replaced by REDACTED, for display in the API, alert emails, and errors. A
// monitored URL may carry a token in its query string, and those places reach
// people who should not see it. Query keys stay visible so the URL is still
// recognizable. Values that are not URLs, such as postgres:$ENV targets, are
// returned unchanged.
func RedactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Opaque != "" {
		if i := strings.IndexAny(rawURL, "?#"); i >= 0 && strings.Contains(rawURL, "://") {
			// Unparseable but URL-shaped: drop everything after the path.
			return rawURL[:i] + "?" + redacted
		}
		return rawURL
	}
	if parsed.User != nil {
		parsed.User = url.User(redacted)
	}
	if parsed.RawQuery != "" {
		parts := strings.Split(parsed.RawQuery, "&")
		for i, part := range parts {
			key, _, hasValue := strings.Cut(part, "=")
			if hasValue {
				parts[i] = key + "=" + redacted
			} else if part != "" {
				// A bare ?token is itself the secret.
				parts[i] = redacted
			}
		}
		parsed.RawQuery = strings.Join(parts, "&")
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	return parsed.String()
}
