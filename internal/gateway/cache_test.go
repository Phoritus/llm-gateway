package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"llm-gateway/internal/cache"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
)

// memoryCache is a ResponseCache a test can read and break.
type memoryCache struct {
	mu      sync.Mutex
	entries map[string]cache.Entry
	failing bool
}

var errCacheDown = errors.New("cache down")

func (m *memoryCache) Get(_ context.Context, key string) (*cache.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing {
		return nil, errCacheDown
	}
	if e, ok := m.entries[key]; ok {
		return &e, nil
	}
	return nil, nil
}

func (m *memoryCache) Put(_ context.Context, key string, e cache.Entry) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing {
		return false, errCacheDown
	}
	m.entries[key] = e
	return true, nil
}

func (m *memoryCache) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func (m *memoryCache) fail(failing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failing = failing
}

// waitStored waits for the write, which runs after the response.
func (m *memoryCache) waitStored(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); m.size() != n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the cache holds %d entries, want %d", m.size(), n)
		}
	}
}

// staysEmpty gives a detached write the time it would need, then checks that
// none happened.
func (m *memoryCache) staysEmpty(t *testing.T) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if n := m.size(); n != 0 {
		t.Fatalf("the cache holds %d entries, want none", n)
	}
}

// cachingRoutes is fakeRoutes with a cache mode, and a version a test can
// bump the way a PUT on the route does.
type cachingRoutes struct {
	mode    store.CacheMode
	version atomic.Int64
}

func (c *cachingRoutes) ResolveRoute(ctx context.Context, workspaceID, alias string) (*store.ResolvedRoute, error) {
	route, err := fakeRoutes{}.ResolveRoute(ctx, workspaceID, alias)
	if err != nil {
		return nil, err
	}
	route.Cache, route.Version = c.mode, c.version.Load()
	return route, nil
}

type cacheFixture struct {
	srv    *httptest.Server
	cache  *memoryCache
	routes *cachingRoutes
	usage  *fakeUsage
	calls  *calls
}

func cacheServer(t *testing.T, mode store.CacheMode, adjust ...func(*Deps)) cacheFixture {
	t.Helper()
	f := cacheFixture{
		cache:  &memoryCache{entries: map[string]cache.Entry{}},
		routes: &cachingRoutes{mode: mode},
		usage:  &fakeUsage{},
		calls:  &calls{},
	}
	f.routes.version.Store(1)
	d := deps(f.calls, time.Minute)
	d.Cache, d.Routes, d.Usage = f.cache, f.routes, f.usage
	for _, a := range adjust {
		a(&d)
	}
	f.srv = httptest.NewServer(http.StripPrefix("/v1", Routes(d)))
	t.Cleanup(f.srv.Close)
	return f
}

func (f cacheFixture) upstreamCalls() int {
	f.calls.mu.Lock()
	defer f.calls.mu.Unlock()
	return len(f.calls.models)
}

// deterministic is chatBody at temperature 0, with any further fields.
func deterministic(model string, stream bool, more map[string]any) string {
	var body map[string]any
	_ = json.Unmarshal([]byte(chatBody(model, stream)), &body)
	body["temperature"] = 0
	for k, v := range more {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func wantCache(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Cache"); got != want {
		t.Fatalf("X-Cache = %q, want %q", got, want)
	}
}

func TestASecondIdenticalRequestIsAnsweredFromTheCache(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)
	body := deterministic("echo", false, nil)

	first := post(t, f.srv, body)
	wantCache(t, first, "MISS")
	wantTrail(t, first, "1", "mock/echo=ok")
	firstBody, _ := io.ReadAll(first.Body)
	f.cache.waitStored(t, 1)

	second := post(t, f.srv, body)
	wantCache(t, second, "HIT")
	wantTrail(t, second, "0", "")
	secondBody, _ := io.ReadAll(second.Body)
	if string(secondBody) != string(firstBody) {
		t.Errorf("the hit answered %s, want the stored %s", secondBody, firstBody)
	}
	if n := f.upstreamCalls(); n != 1 {
		t.Errorf("the upstream was called %d times, want once", n)
	}

	events := recordedEvents(t, f.usage, 2)
	miss, hit := events[0], events[1]
	if miss.Cached || miss.PromptTokens == 0 || miss.Attempts != 1 {
		t.Errorf("the miss was recorded as %+v", miss)
	}
	if !hit.Cached || hit.PromptTokens != 0 || hit.CompletionTokens != 0 || hit.Attempts != 0 {
		t.Errorf("the hit was recorded as %+v, want cached with no tokens and no attempts", hit)
	}
	if hit.Vendor != "mock" || hit.Model != "echo" || hit.Status != http.StatusOK || hit.Outcome != store.OutcomeOK {
		t.Errorf("the hit was recorded as %+v, want the target that produced the stored answer", hit)
	}
}

func TestARequestThatDiffersOnlyInFormIsTheSameRequest(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)
	drain(post(t, f.srv, deterministic("echo", false, nil)))
	f.cache.waitStored(t, 1)

	respaced := `{ "temperature": 0.0, "user": "someone", "presence_penalty": 0.5,
		"messages": [ {"content": "one two three four", "role": "user"} ], "model": "echo" }`
	wantCache(t, post(t, f.srv, respaced), "HIT")
}

func streamedText(t *testing.T, ev []string) string {
	t.Helper()
	if len(ev) < 2 || ev[len(ev)-1] != "[DONE]" {
		t.Fatalf("events = %v, want chunks then [DONE]", ev)
	}
	var text strings.Builder
	for _, e := range ev[:len(ev)-1] {
		var c provider.ChatChunk
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			t.Fatalf("chunk %q: %v", e, err)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	return text.String()
}

func TestAStreamedHitReplaysTheStoredAnswer(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)
	drain(post(t, f.srv, deterministic("echo", false, nil)))
	f.cache.waitStored(t, 1)

	resp := post(t, f.srv, deterministic("echo", true, nil))
	wantCache(t, resp, "HIT")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	ev := events(t, resp.Body)
	if got := streamedText(t, ev); got != "one two three four" {
		t.Errorf("replayed text = %q", got)
	}
	for _, e := range ev[:len(ev)-1] {
		if strings.Contains(e, `"usage"`) {
			t.Errorf("the client was sent %s without asking for usage", e)
		}
	}
	var last provider.ChatChunk
	_ = json.Unmarshal([]byte(ev[len(ev)-2]), &last)
	if len(last.Choices) != 1 || last.Choices[0].FinishReason == nil {
		t.Errorf("last chunk = %s, want the finish reason", ev[len(ev)-2])
	}

	withUsage := post(t, f.srv, deterministic("echo", true, map[string]any{
		"stream_options": map[string]any{"include_usage": true},
	}))
	wantCache(t, withUsage, "HIT")
	ev = events(t, withUsage.Body)
	_ = json.Unmarshal([]byte(ev[len(ev)-2]), &last)
	if last.Usage == nil || last.Usage.TotalTokens != echoTokens {
		t.Errorf("last chunk = %s, want the stored usage", ev[len(ev)-2])
	}
	if n := f.upstreamCalls(); n != 1 {
		t.Errorf("the upstream was called %d times, want once", n)
	}
}

func TestAStreamedAnswerIsStoredForTheNextRequest(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)
	resp := post(t, f.srv, deterministic("echo", true, nil))
	wantCache(t, resp, "MISS")
	drain(resp)
	f.cache.waitStored(t, 1)

	hit := post(t, f.srv, deterministic("echo", false, nil))
	wantCache(t, hit, "HIT")
	if got := completionText(t, hit.Body); got != "one two three four" {
		t.Errorf("hit content = %q", got)
	}
}

func TestAHitSpendsARequestAndNoTokens(t *testing.T) {
	keyTokens, keyRequests := tight(1000), tight(10)
	limiter := ratelimit.NewLocal()
	f := cacheServer(t, store.CacheAuto, func(d *Deps) {
		d.Limiter, d.Limits.KeyTokens, d.Limits.KeyRequests = limiter, keyTokens, keyRequests
	})
	body := deterministic("echo", false, nil)

	drain(post(t, f.srv, body))
	f.cache.waitStored(t, 1)
	waitForLevel(t, limiter, scopeKeyTokens, "k1", keyTokens, 1000-echoTokens)

	wantCache(t, post(t, f.srv, body), "HIT")
	recordedEvents(t, f.usage, 2)
	// The hit's reservation is given back whole, so only the miss was charged.
	waitForLevel(t, limiter, scopeKeyTokens, "k1", keyTokens, 1000-echoTokens)
	waitForLevel(t, limiter, scopeKeyRequests, "k1", keyRequests, 8)
}

func TestToolRequestsAreNeverCached(t *testing.T) {
	f := cacheServer(t, store.CacheAlways)
	body := deterministic("echo", false, map[string]any{
		"tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup"}}},
	})

	for range 2 {
		resp := post(t, f.srv, body)
		wantCache(t, resp, "")
		drain(resp)
	}
	f.cache.staysEmpty(t)
	if n := f.upstreamCalls(); n != 2 {
		t.Errorf("the upstream was called %d times, want every time", n)
	}
}

func TestARouteWithCachingOffNeverCaches(t *testing.T) {
	f := cacheServer(t, store.CacheOff)
	for range 2 {
		resp := post(t, f.srv, deterministic("echo", false, nil))
		wantCache(t, resp, "")
		drain(resp)
	}
	f.cache.staysEmpty(t)
	if n := f.upstreamCalls(); n != 2 {
		t.Errorf("the upstream was called %d times, want every time", n)
	}
}

func TestAutoCachesOnlyAtTemperatureZero(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)
	for range 2 {
		resp := post(t, f.srv, chatBody("echo", false))
		wantCache(t, resp, "")
		drain(resp)
	}
	f.cache.staysEmpty(t)
}

func TestAlwaysCachesAtAnyTemperature(t *testing.T) {
	f := cacheServer(t, store.CacheAlways)
	wantCache(t, post(t, f.srv, chatBody("echo", false)), "MISS")
	f.cache.waitStored(t, 1)
	wantCache(t, post(t, f.srv, chatBody("echo", false)), "HIT")
}

func TestARewrittenRouteMissesWhatItAnsweredBefore(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)
	body := deterministic("echo", false, nil)
	drain(post(t, f.srv, body))
	f.cache.waitStored(t, 1)
	wantCache(t, post(t, f.srv, body), "HIT")

	f.routes.version.Add(1)
	wantCache(t, post(t, f.srv, body), "MISS")
	if n := f.upstreamCalls(); n != 2 {
		t.Errorf("the upstream was called %d times, want once per version of the route", n)
	}
}

func TestACacheThatFailsIsAMiss(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)
	body := deterministic("echo", false, nil)
	drain(post(t, f.srv, body))
	f.cache.waitStored(t, 1)

	f.cache.fail(true)
	resp := post(t, f.srv, body)
	wantCache(t, resp, "MISS")
	if got := completionText(t, resp.Body); got != "one two three four" {
		t.Errorf("content = %q, want the upstream's answer", got)
	}
	if n := f.upstreamCalls(); n != 2 {
		t.Errorf("the upstream was called %d times, want it called when the cache could not answer", n)
	}
}

func TestAnAnswerThatFailedOrBrokeIsNotCached(t *testing.T) {
	f := cacheServer(t, store.CacheAuto)

	if resp := post(t, f.srv, deterministic("fail", false, nil)); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("failing upstream: status = %d", resp.StatusCode)
	}
	if resp := post(t, f.srv, deterministic("break", false, nil)); resp.StatusCode == http.StatusOK {
		t.Fatalf("a non-streaming answer that broke was sent as a success")
	}
	broken := post(t, f.srv, deterministic("break", true, nil))
	ev := events(t, broken.Body)
	if len(ev) == 0 || ev[len(ev)-1] == "[DONE]" {
		t.Fatalf("events = %v, want a stream that ends with an error", ev)
	}
	f.cache.staysEmpty(t)
}

func cacheCounts(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gateway.cache" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if dp.Attributes.Len() != 1 {
					t.Errorf("gateway.cache has a series with %d labels, want result only", dp.Attributes.Len())
				}
				result, _ := dp.Attributes.Value("result")
				out[result.AsString()] = dp.Value
			}
		}
	}
	return out
}

func TestAHitIsCountedUnderTheCacheAndNotUnderAVendor(t *testing.T) {
	metricReader()
	f := cacheServer(t, store.CacheAuto)
	body := deterministic("echo", false, nil)
	requestsBefore, cacheBefore := counterSeries(t, "gateway.requests"), cacheCounts(t)

	drain(post(t, f.srv, body))
	f.cache.waitStored(t, 1)
	drain(post(t, f.srv, body))
	drain(post(t, f.srv, chatBody("echo", false)))
	recordedEvents(t, f.usage, 3)

	requests := counterSeries(t, "gateway.requests")
	if got := requests["cache/ok"] - requestsBefore["cache/ok"]; got != 1 {
		t.Errorf("cache/ok moved by %d, want 1", got)
	}
	if got := requests["mock/ok"] - requestsBefore["mock/ok"]; got != 2 {
		t.Errorf("mock/ok moved by %d, want the two requests the upstream answered", got)
	}
	after := cacheCounts(t)
	for _, result := range []string{cacheHit, cacheMiss, cacheStored, cacheSkipped} {
		if got := after[result] - cacheBefore[result]; got != 1 {
			t.Errorf("gateway.cache %s moved by %d, want 1", result, got)
		}
	}
}
