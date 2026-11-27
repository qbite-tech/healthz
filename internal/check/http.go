package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qbite-tech/healthz/internal/config"
)

const maxBodyBytes = 1 << 20

type Result struct {
	Healthy    bool
	StatusCode int
	Duration   time.Duration
	Err        error
}

type Checker interface {
	Check(context.Context, config.EndpointConfig) Result
}

type HTTPChecker struct {
	client *http.Client
}

func NewHTTPChecker() *HTTPChecker {
	return &HTTPChecker{client: &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if !safeRedirect(via[0].URL, request.URL) {
				// Go itself drops only Authorization and Cookie here. Configured
				// headers often carry API keys, so keep them away from other hosts
				// and from plain HTTP.
				for _, name := range configuredHeaders(request.Context()) {
					request.Header.Del(name)
				}
			}
			return nil
		},
	}}
}

type configuredHeadersKey struct{}

// configuredHeaders returns the names of the endpoint's configured headers
// stored in the request context by Check.
func configuredHeaders(ctx context.Context) []string {
	names, _ := ctx.Value(configuredHeadersKey{}).([]string)
	return names
}

// safeRedirect reports whether configured headers may follow a redirect from
// the original URL to target: the target must be the same host or one of its
// subdomains (the rule Go applies to Authorization), and must not downgrade
// HTTPS to HTTP.
func safeRedirect(original, target *url.URL) bool {
	if original.Scheme == "https" && target.Scheme != "https" {
		return false
	}
	originalHost := strings.ToLower(original.Hostname())
	targetHost := strings.ToLower(target.Hostname())
	return targetHost == originalHost || strings.HasSuffix(targetHost, "."+originalHost)
}

func (c *HTTPChecker) Check(parent context.Context, endpoint config.EndpointConfig) Result {
	ctx, cancel := context.WithTimeout(parent, endpoint.Timeout.Duration)
	defer cancel()
	headerNames := make([]string, 0, len(endpoint.Headers))
	for name := range endpoint.Headers {
		headerNames = append(headerNames, name)
	}
	ctx = context.WithValue(ctx, configuredHeadersKey{}, headerNames)

	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, endpoint.Method, endpoint.URL, nil)
	if err != nil {
		return Result{Duration: time.Since(started), Err: fmt.Errorf("build request: %w", redactURLError(err))}
	}
	request.Header.Set("User-Agent", "healthz-monitor/1.0")
	for key, value := range endpoint.Headers {
		request.Header.Set(key, value)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return Result{Duration: time.Since(started), Err: fmt.Errorf("request failed: %w", redactURLError(err))}
	}
	defer response.Body.Close()

	if !accepted(response.StatusCode, endpoint.ExpectedStatuses) {
		return Result{
			StatusCode: response.StatusCode,
			Duration:   time.Since(started),
			Err:        fmt.Errorf("unexpected HTTP status %d", response.StatusCode),
		}
	}
	if endpoint.BodyContains != "" && endpoint.Method != http.MethodHead {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
		if readErr != nil {
			return Result{StatusCode: response.StatusCode, Duration: time.Since(started), Err: fmt.Errorf("read response: %w", readErr)}
		}
		if len(body) > maxBodyBytes {
			return Result{StatusCode: response.StatusCode, Duration: time.Since(started), Err: fmt.Errorf("response exceeds %d bytes", maxBodyBytes)}
		}
		if !strings.Contains(string(body), endpoint.BodyContains) {
			return Result{StatusCode: response.StatusCode, Duration: time.Since(started), Err: fmt.Errorf("response does not contain required text %q", endpoint.BodyContains)}
		}
	}

	return Result{Healthy: true, StatusCode: response.StatusCode, Duration: time.Since(started)}
}

// redactURLError masks query values in the URL that net/http and net/url
// errors embed, because check errors are stored, served by the API, and
// emailed.
func redactURLError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		urlError.URL = config.RedactURL(urlError.URL)
	}
	return err
}

func accepted(actual int, expected []int) bool {
	if len(expected) == 0 {
		return actual >= 200 && actual <= 399
	}
	for _, status := range expected {
		if actual == status {
			return true
		}
	}
	return false
}
