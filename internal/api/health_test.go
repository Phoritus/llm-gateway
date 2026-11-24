package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHealthzSaysDrainingOnceShutdownStarts(t *testing.T) {
	var draining atomic.Bool
	mux := http.NewServeMux()
	Health(nil, draining.Load)(mux)
	status := func() int {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return w.Code
	}
	if got := status(); got != http.StatusOK {
		t.Fatalf("before shutdown: %d, want 200", got)
	}
	draining.Store(true)
	if got := status(); got != http.StatusServiceUnavailable {
		t.Fatalf("draining: %d, want 503", got)
	}
}
