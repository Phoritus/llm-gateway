package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader is the header a request id is read from and returned in.
const RequestIDHeader = "X-Request-Id"

// maxInboundRequestID bounds an id we are willing to echo. An id travels into
// logs and back out in a header, so an unbounded caller-supplied value is a
// log-injection and header-smuggling vector.
const maxInboundRequestID = 64

type contextKey int

const requestIDKey contextKey = iota

// RequestID attaches a request id to every request, reusing an inbound one
// only when it is short and made of characters that cannot break a log line
// or a header. The id is echoed in the response so a client can quote it in a
// bug report.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

func sanitizeRequestID(raw string) string {
	if raw == "" || len(raw) > maxInboundRequestID {
		return ""
	}
	for _, c := range raw {
		isAllowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !isAllowed {
			return ""
		}
	}
	return raw
}

func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// An id is for correlation only, so a timestamp is an acceptable
		// fallback rather than a reason to fail the request.
		return "ts-" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(buf)
}

// WithRequestID attaches an id to a context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request id, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusRecorder captures the status code and response size for the access
// log, since http.ResponseWriter does not expose them.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush forwards to the wrapped writer when it supports flushing, which the
// streaming data plane depends on. Dropping this would buffer /v1 responses.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets the standard library's response controller reach the
// underlying writer for hijacking and deadlines.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog logs one line per request.
//
// It deliberately logs no headers, no cookies and no bodies. Everything
// sensitive this service handles travels in exactly those places: the session
// cookie, the gateway key in Authorization, the password in a body, the
// verification token in a query string. Logging the method, path, status and
// duration is enough to operate the service and carries none of it.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			// The path is logged without its query string, because tokens
			// arrive there.
			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			}
			if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
				attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
			}
			logger.LogAttrs(r.Context(), levelFor(rec.status), "request", attrs...)
		})
	}
}

func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
