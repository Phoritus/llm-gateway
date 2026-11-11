package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"llm-gateway/internal/config"
	"llm-gateway/internal/telemetry"
)

// instrumented builds the same handler shape main does -- otelhttp outside,
// telemetry.Route around the mux -- with in-memory exporters.
func instrumented(t *testing.T, api http.Handler) (http.Handler, *sdkmetric.ManualReader, *tracetest.SpanRecorder) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	mux := http.NewServeMux()
	Health(nil)(mux)
	mux.Handle("/api/", http.StripPrefix("/api", api))

	h := otelhttp.NewHandler(telemetry.Route(mux), "http.server",
		otelhttp.WithMeterProvider(mp), otelhttp.WithTracerProvider(tp))
	return h, reader, recorder
}

// routes returns the http.route value of every series otelhttp recorded for
// its request duration histogram.
func routes(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is a %T, want a float64 histogram", m.Name, m.Data)
			}
			for _, dp := range hist.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("http.route"))
				out = append(out, v.AsString())
			}
		}
	}
	return out
}

func TestRouteLabelIsThePatternNotThePath(t *testing.T) {
	r := chi.NewRouter()
	r.Use(routeTelemetry)
	r.Get("/things/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h, reader, recorder := instrumented(t, r)

	for _, path := range []string{
		"/api/things/11111111-aaaa", "/api/things/22222222-bbbb",
		"/healthz",
		"/api/no/such/route", "/elsewhere/entirely",
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	got := map[string]bool{}
	for _, route := range routes(t, reader) {
		got[route] = true
	}
	want := map[string]bool{"/api/things/{id}": true, "/healthz": true, telemetry.UnmatchedRoute: true}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want exactly %v", got, want)
	}
	for route := range want {
		if !got[route] {
			t.Errorf("no series for route %q; have %v", route, got)
		}
	}

	for _, span := range recorder.Ended() {
		if strings.Contains(span.Name(), "1111") || strings.Contains(span.Name(), "no/such") || strings.Contains(span.Name(), "elsewhere") {
			t.Errorf("span name %q carries a raw path", span.Name())
		}
	}
	names := map[string]bool{}
	for _, span := range recorder.Ended() {
		names[span.Name()] = true
	}
	if !names["GET /api/things/{id}"] {
		t.Errorf("span names = %v, want one named after the pattern", names)
	}
}

// The real router, not a stand-in. An unknown path under /api is "unmatched",
// and one under a sub-router chi reports that sub-router's wildcard. Either
// way the label comes from a fixed set, never from the request.
func TestRealRoutesReportBoundedRoutes(t *testing.T) {
	h, reader, _ := instrumented(t, Routes(Deps{Config: &config.Config{}}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/definitely/not/here/12345", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/auth/verify-email-nope", nil))

	for _, route := range routes(t, reader) {
		if route != telemetry.UnmatchedRoute && route != "/api/auth/*" {
			t.Errorf("route = %q for an unknown path, want %q or a sub-router wildcard", route, telemetry.UnmatchedRoute)
		}
	}
}
