package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

// maxBodyBytes caps a /v1 request body. A long conversation is large -- a
// 128k-token context is around half a megabyte of text -- so this is far above
// the control plane's cap, and still small enough that the cap is a limit.
const maxBodyBytes = 4 << 20

// Deps is everything the data plane needs.
type Deps struct {
	Keys      KeyAuthenticator
	Routes    RouteResolver
	Providers map[provider.Vendor]provider.Provider
	// Timeout is the deadline on one request, streaming included.
	Timeout time.Duration
	Logger  *slog.Logger
}

// KeyAuthenticator resolves a presented gateway key, returning
// auth.ErrInvalidGatewayKey for a key that is malformed, unknown or revoked.
type KeyAuthenticator interface {
	Authenticate(ctx context.Context, token string) (store.ActiveGatewayKey, error)
}

// RouteResolver resolves a request's model to the workspace's route,
// returning store.ErrNotFound for an alias the workspace does not have.
type RouteResolver interface {
	ResolveRoute(ctx context.Context, workspaceID, alias string) (*store.ResolvedRoute, error)
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Routes builds the /v1 handler. It expects the /v1 prefix stripped.
func Routes(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(routeTelemetry)
	r.Post("/chat/completions", d.handleChatCompletions)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, typeNotFound, "not_found",
			"The requested resource does not exist.")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, typeInvalidRequest, "method_not_allowed",
			"That method is not allowed on this endpoint.")
	})
	return r
}

// routeTelemetry reports chi's matched pattern, never the raw path, for the
// same reason the control plane does: a path-derived label mints a metric
// series per distinct path.
func routeTelemetry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		route := telemetry.UnmatchedRoute
		if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
			route = "/v1" + pattern
		}
		telemetry.SetRoute(r.Context(), route)
	})
}
