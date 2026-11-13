package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/store"
	"llm-gateway/internal/usage"
)

func usageServer(t *testing.T, adjust ...func(*Deps)) (*httptest.Server, *fakeUsage) {
	t.Helper()
	rec := &fakeUsage{}
	d := deps(&mock.Provider{}, time.Minute)
	d.Usage = rec
	for _, f := range adjust {
		f(&d)
	}
	srv := httptest.NewServer(http.StripPrefix("/v1", Routes(d)))
	t.Cleanup(srv.Close)
	return srv, rec
}

// recordedEvents waits for n events: the handler records after the response
// has been sent, so the client can read the body first.
func recordedEvents(t *testing.T, rec *fakeUsage, n int) []usage.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.recorded()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d events, want %d", len(rec.recorded()), n)
		}
		time.Sleep(time.Millisecond)
	}
	// Anything more would have arrived by now.
	time.Sleep(20 * time.Millisecond)
	events := rec.recorded()
	if len(events) != n {
		t.Fatalf("recorded %d events, want %d: %+v", len(events), n, events)
	}
	return events
}

func TestASuccessfulRequestRecordsOneEventWithItsTokens(t *testing.T) {
	srv, rec := usageServer(t)
	drain(post(t, srv, chatBody("mock/echo", false)))

	e := recordedEvents(t, rec, 1)[0]
	if e.WorkspaceID != "ws1" || e.KeyID != "k1" || e.RouteAlias != "mock/echo" {
		t.Errorf("identity = %s %s %s", e.WorkspaceID, e.KeyID, e.RouteAlias)
	}
	if e.Vendor != "mock" || e.Model == "" {
		t.Errorf("target = %s/%s", e.Vendor, e.Model)
	}
	if e.Status != http.StatusOK || e.Outcome != store.OutcomeOK || e.Attempts != 1 {
		t.Errorf("status %d outcome %s attempts %d", e.Status, e.Outcome, e.Attempts)
	}
	if e.PromptTokens == 0 || e.CompletionTokens == 0 {
		t.Errorf("tokens = %d/%d, want the reported usage", e.PromptTokens, e.CompletionTokens)
	}
	if e.CreatedAt.IsZero() || e.TTFTMs > e.LatencyMs {
		t.Errorf("timing = created %s ttft %dms latency %dms", e.CreatedAt, e.TTFTMs, e.LatencyMs)
	}
}

func TestAnExhaustedRouteRecordsOneEventWithoutAVendor(t *testing.T) {
	srv, rec := usageServer(t)
	resp := post(t, srv, chatBody("fail,rate-limited", false))
	drain(resp)

	e := recordedEvents(t, rec, 1)[0]
	if e.Vendor != "" || e.Model != "" {
		t.Errorf("target = %s/%s, want none", e.Vendor, e.Model)
	}
	if e.Status != resp.StatusCode || e.Status != http.StatusTooManyRequests ||
		e.Outcome != "rate_limited" || e.Attempts != 2 {
		t.Errorf("status %d outcome %s attempts %d", e.Status, e.Outcome, e.Attempts)
	}
	if e.PromptTokens != 0 || e.TTFTMs != 0 {
		t.Errorf("an unanswered request has tokens %d or a first token at %dms", e.PromptTokens, e.TTFTMs)
	}
}

func TestAStreamThatBreaksAfterItsFirstChunkIsOkToTheClientAndAStreamErrorToUs(t *testing.T) {
	srv, rec := usageServer(t)
	drain(post(t, srv, chatBody("break", true)))

	e := recordedEvents(t, rec, 1)[0]
	if e.Status != http.StatusOK || e.Outcome != store.OutcomeStreamError || e.Vendor != "mock" {
		t.Errorf("status %d outcome %s vendor %s", e.Status, e.Outcome, e.Vendor)
	}
}

func TestRequestsRefusedBeforeTheRouteLeaveNoEvent(t *testing.T) {
	srv, rec := usageServer(t)
	drain(postAs(t, srv, "Bearer wrong", chatBody("mock/echo", false)))
	drain(post(t, srv, chatBody(missingAlias, false)))
	drain(post(t, srv, "not json"))
	time.Sleep(50 * time.Millisecond)
	if events := rec.recorded(); len(events) != 0 {
		t.Errorf("refused requests recorded %+v", events)
	}
}

func TestARequestRefusedByTheGatewaysOwnLimitLeavesNoEvent(t *testing.T) {
	srv, rec := usageServer(t, func(d *Deps) { d.Limits.KeyRequests = tight(1) })
	drain(post(t, srv, chatBody("mock/echo", false)))
	if resp := post(t, srv, chatBody("mock/echo", false)); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the gateway's own limit", resp.StatusCode)
	}
	recordedEvents(t, rec, 1)
}

var metricReader = sync.OnceValue(func() *sdkmetric.ManualReader {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
})

// series is every data point of a counter, keyed by its attributes.
func counterSeries(t *testing.T, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if dp.Attributes.Len() != 2 {
					t.Errorf("%s has a series with %d labels, want vendor and outcome only", name, dp.Attributes.Len())
				}
				v, _ := dp.Attributes.Value("vendor")
				o, _ := dp.Attributes.Value("outcome")
				out[v.AsString()+"/"+o.AsString()] = dp.Value
			}
		}
	}
	return out
}

func TestMetricsAreLabelledByVendorAndOutcomeAlone(t *testing.T) {
	metricReader()
	before := counterSeries(t, "gateway.requests")
	beforeErrors := counterSeries(t, "router.upstream.errors")
	srv, rec := usageServer(t)

	drain(post(t, srv, chatBody("fail,echo", false)))
	drain(post(t, srv, chatBody(missingAlias, false)))
	recordedEvents(t, rec, 1)
	deadline := time.Now().Add(5 * time.Second)
	for counterSeries(t, "gateway.requests")["none/refused"] == before["none/refused"] {
		if time.Now().After(deadline) {
			t.Fatal("the refused request was never counted")
		}
		time.Sleep(time.Millisecond)
	}

	after := counterSeries(t, "gateway.requests")
	if after["mock/ok"]-before["mock/ok"] != 1 {
		t.Errorf("mock/ok moved by %d, want 1", after["mock/ok"]-before["mock/ok"])
	}
	if after["none/refused"]-before["none/refused"] != 1 {
		t.Errorf("none/refused moved by %d, want 1", after["none/refused"]-before["none/refused"])
	}
	afterErrors := counterSeries(t, "router.upstream.errors")
	if afterErrors["mock/upstream_error"]-beforeErrors["mock/upstream_error"] != 1 {
		t.Errorf("router.upstream.errors = %v, want one mock/upstream_error", afterErrors)
	}
}
