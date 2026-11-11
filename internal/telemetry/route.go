package telemetry

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// UnmatchedRoute is the route reported for any request no handler claimed.
const UnmatchedRoute = "unmatched"

type routeKey struct{}

type routeHolder struct{ route string }

// SetRoute records the route pattern a request matched, for Route to report.
// A sub-router that owns its own patterns calls it once the request has
// finished routing, since only then is the full pattern known.
func SetRoute(ctx context.Context, pattern string) {
	if h, ok := ctx.Value(routeKey{}).(*routeHolder); ok {
		h.route = pattern
	}
}

// Route labels the surrounding otelhttp metrics and span with a bounded
// http.route. It wraps the ServeMux directly, because the mux records the
// matched pattern on the very request it was handed, which a middleware
// further out would never see.
//
// The value is always a registered pattern or UnmatchedRoute, never the
// request path: a path containing an id would mint a new metric series per
// id, and Grafana Cloud's free tier stops accepting data at 10k series.
func Route(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := &routeHolder{}
		inner := r.WithContext(context.WithValue(r.Context(), routeKey{}, h))
		next.ServeHTTP(w, inner)

		route := h.route
		if route == "" {
			route = muxPattern(inner.Pattern)
		}
		if route == "" {
			route = UnmatchedRoute
		}

		ctx := r.Context()
		if l, ok := otelhttp.LabelerFromContext(ctx); ok {
			l.Add(attribute.String("http.route", route))
		}
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(attribute.String("http.route", route))
		span.SetName(r.Method + " " + route)
	})
}

// muxPattern strips the method from a ServeMux pattern ("GET /healthz").
func muxPattern(pattern string) string {
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		return rest
	}
	return pattern
}
