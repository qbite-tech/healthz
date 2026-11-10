package check

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/runeQ/healthz/internal/config"
)

func TestHTTPCheckerHealthyResponse(t *testing.T) {
	checker := fakeHTTPChecker(func(request *http.Request) *http.Response {
		if request.Header.Get("X-Monitor-Token") != "secret" {
			t.Error("custom request header was not sent")
		}
		return response(http.StatusOK, "service healthy")
	})

	result := checker.Check(context.Background(), endpoint("https://example.test", []int{200}, "service healthy"))
	if !result.Healthy || result.Err != nil {
		t.Fatalf("Check() = %+v, want healthy", result)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", result.StatusCode)
	}
}

func TestHTTPCheckerRejectsUnexpectedStatus(t *testing.T) {
	checker := fakeHTTPChecker(func(_ *http.Request) *http.Response {
		return response(http.StatusServiceUnavailable, "unavailable")
	})

	result := checker.Check(context.Background(), endpoint("https://example.test", []int{200}, ""))
	if result.Healthy || result.Err == nil || !strings.Contains(result.Err.Error(), "503") {
		t.Fatalf("Check() = %+v, want unhealthy status error", result)
	}
}

func TestHTTPCheckerChecksBody(t *testing.T) {
	checker := fakeHTTPChecker(func(_ *http.Request) *http.Response {
		return response(http.StatusOK, "wrong response")
	})

	result := checker.Check(context.Background(), endpoint("https://example.test", nil, "ready"))
	if result.Healthy || result.Err == nil || !strings.Contains(result.Err.Error(), "required text") {
		t.Fatalf("Check() = %+v, want missing body text error", result)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func fakeHTTPChecker(handler func(*http.Request) *http.Response) *HTTPChecker {
	return &HTTPChecker{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return handler(request), nil
	})}}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func endpoint(url string, statuses []int, body string) config.EndpointConfig {
	return config.EndpointConfig{
		Name:             "test",
		URL:              url,
		Method:           http.MethodGet,
		Timeout:          config.Duration{Duration: time.Second},
		ExpectedStatuses: statuses,
		BodyContains:     body,
		Headers:          map[string]string{"X-Monitor-Token": "secret"},
	}
}

// redirectingChecker uses the production redirect policy with a fake network:
// the first request is redirected to target, and every request's
// X-Monitor-Token header is recorded by URL.
func redirectingChecker(target string, seen map[string]string) *HTTPChecker {
	checker := NewHTTPChecker()
	checker.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen[request.URL.String()] = request.Header.Get("X-Monitor-Token")
		if request.URL.String() == "https://example.test/health" {
			redirect := response(http.StatusFound, "")
			redirect.Header.Set("Location", target)
			return redirect, nil
		}
		return response(http.StatusOK, "ready"), nil
	})
	return checker
}

func TestHTTPCheckerKeepsHeadersOnSafeRedirects(t *testing.T) {
	for _, target := range []string{
		"https://example.test/moved",
		"https://www.example.test/health",
		"https://EXAMPLE.test:8443/health",
	} {
		seen := map[string]string{}
		result := redirectingChecker(target, seen).Check(context.Background(), endpoint("https://example.test/health", nil, ""))
		if !result.Healthy {
			t.Fatalf("redirect to %s: Check() = %+v, want healthy", target, result)
		}
		if seen[target] != "secret" {
			t.Errorf("redirect to %s dropped the configured header", target)
		}
	}
}

func TestHTTPCheckerDropsHeadersOnUnsafeRedirects(t *testing.T) {
	for _, target := range []string{
		"https://attacker.test/collect",
		"https://example.test.attacker.test/collect",
		"http://example.test/health",
	} {
		seen := map[string]string{}
		redirectingChecker(target, seen).Check(context.Background(), endpoint("https://example.test/health", nil, ""))
		if value, requested := seen[target]; !requested || value != "" {
			t.Errorf("redirect to %s: requested=%v header=%q, want request without the configured header",
				target, requested, value)
		}
	}
}

func TestHTTPCheckerRedactsURLInErrors(t *testing.T) {
	checker := NewHTTPChecker()
	checker.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	result := checker.Check(context.Background(), endpoint("https://example.test/health?token=s3cret", nil, ""))
	if result.Err == nil {
		t.Fatal("Check() succeeded, want error")
	}
	if message := result.Err.Error(); strings.Contains(message, "s3cret") || !strings.Contains(message, "token=REDACTED") {
		t.Fatalf("error = %q, want the query value redacted", message)
	}
}
