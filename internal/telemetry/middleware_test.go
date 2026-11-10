package telemetry

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratesAndEchoes(t *testing.T) {
	var seen string
	handler := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if seen == "" {
		t.Fatal("no request id in the handler's context")
	}
	if got := w.Header().Get(RequestIDHeader); got != seen {
		t.Fatalf("response header = %q, want the context id %q", got, seen)
	}
}

func TestRequestIDSanitizesInboundValues(t *testing.T) {
	cases := []struct {
		name     string
		inbound  string
		wantKept bool
	}{
		{"plain", "abc123", true},
		{"with dashes and underscores", "req-123_456", true},
		{"with a space", "req 123", false},
		{"with a newline", "req\n123", false},
		{"with a quote", `req"123`, false},
		{"with a semicolon", "req;123", false},
		{"too long", strings.Repeat("a", maxInboundRequestID+1), false},
		{"at the length limit", strings.Repeat("a", maxInboundRequestID), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			handler := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
			}))

			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			r.Header.Set(RequestIDHeader, tc.inbound)
			handler.ServeHTTP(httptest.NewRecorder(), r)

			if tc.wantKept && seen != tc.inbound {
				t.Fatalf("id = %q, want the inbound %q", seen, tc.inbound)
			}
			// A rejected id must be replaced, never echoed: it ends up in a
			// log line and a response header.
			if !tc.wantKept && seen == tc.inbound {
				t.Fatalf("id %q was echoed although it should have been replaced", tc.inbound)
			}
		})
	}
}

// The access log is the one place a secret could leak into persistent
// storage, so this asserts what it does not contain.
func TestAccessLogOmitsSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := RequestID(AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("body"))
	})))

	r := httptest.NewRequest(http.MethodPost, "/api/auth/verify-email?token=super-secret-token", nil)
	r.Header.Set("Authorization", "Bearer gw_live_secret")
	r.Header.Set("Cookie", "gw_session=another-secret")
	handler.ServeHTTP(httptest.NewRecorder(), r)

	line := buf.String()
	for _, secret := range []string{"super-secret-token", "gw_live_secret", "another-secret", "Authorization", "Cookie"} {
		if strings.Contains(line, secret) {
			t.Errorf("access log contains %q:\n%s", secret, line)
		}
	}
	// It must still be useful.
	for _, want := range []string{`"method":"POST"`, `"path":"/api/auth/verify-email"`, `"status":418`, `"request_id"`} {
		if !strings.Contains(line, want) {
			t.Errorf("access log is missing %s:\n%s", want, line)
		}
	}
}

// The data plane streams responses, so the wrapper must not swallow Flush.
func TestAccessLogPreservesFlusher(t *testing.T) {
	var flushable bool
	handler := AccessLog(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, flushable = w.(http.Flusher)
		}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))

	if !flushable {
		t.Fatal("the wrapped ResponseWriter does not implement http.Flusher")
	}
}
