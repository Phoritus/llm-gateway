package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

type healthBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Check is one dependency the readiness probe reports on.
//
// The list is owned by whoever wires the service up, not discovered by this
// package through a field on Deps. That is the version that still works when
// /v1 adds a cache and a usage buffer: each gets a line where the clients are
// opened, rather than another dependency threaded into the control plane's
// handler struct so that one probe can reach it.
type Check struct {
	Name string
	Ping func(context.Context) error
}

// Health mounts the liveness and readiness probes at the server root, outside
// /api, because an orchestrator calls them without a session and they are not
// part of the control plane's CORS or CSRF surface. draining, when it reports
// true, turns /healthz to 503; nil never does.
func Health(logger *slog.Logger, draining func() bool, checks ...Check) func(mux *http.ServeMux) {
	if logger == nil {
		logger = slog.Default()
	}
	return func(mux *http.ServeMux) {
		mux.HandleFunc("GET /healthz", handleServing(draining))
		mux.HandleFunc("GET /readyz", handleReady(logger, checks))
	}
}

func handleServing(draining func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if draining != nil && draining() {
			writeJSON(w, http.StatusServiceUnavailable, healthBody{Status: "draining"})
			return
		}
		writeJSON(w, http.StatusOK, healthBody{Status: "ok"})
	}
}

// handleReady reports whether this instance can serve traffic, which means
// every dependency it was given answers.
func handleReady(logger *slog.Logger, checks []Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		results := make(map[string]string, len(checks))
		ready := true
		for _, check := range checks {
			if err := check.Ping(ctx); err != nil {
				// The reason is logged, not returned: the probe is
				// unauthenticated, and a connection string or host name in the
				// body would be a free hint about the infrastructure.
				logger.ErrorContext(ctx, "readiness check failed",
					slog.String("dependency", check.Name))
				results[check.Name] = "unreachable"
				ready = false
				continue
			}
			results[check.Name] = "ok"
		}

		status, body := http.StatusOK, "ready"
		if !ready {
			status, body = http.StatusServiceUnavailable, "not_ready"
		}
		writeJSON(w, status, healthBody{Status: body, Checks: results})
	}
}
