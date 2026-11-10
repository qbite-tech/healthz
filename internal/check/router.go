package check

import (
	"context"
	"fmt"

	"github.com/runeQ/healthz/internal/config"
)

// Router sends each endpoint to the checker for its configured type.
type Router struct {
	checkers map[string]Checker
}

// NewRouter registers every checker. pings supplies recorded pings to ping
// endpoints and may be nil when none are configured.
func NewRouter(pings PingStateSource) *Router {
	return &Router{checkers: map[string]Checker{
		config.EndpointTypeHTTP:     NewHTTPChecker(),
		config.EndpointTypePostgres: NewPostgresChecker(),
		config.EndpointTypePing:     NewPingChecker(pings),
	}}
}

func (r *Router) Check(ctx context.Context, endpoint config.EndpointConfig) Result {
	checker, ok := r.checkers[endpoint.Type]
	if !ok {
		return Result{Err: fmt.Errorf("unsupported endpoint type %q", endpoint.Type)}
	}
	return checker.Check(ctx, endpoint)
}
