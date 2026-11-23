package gateway

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/cache"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/store"
)

const otherKey = "gwk_other"

type twoWorkspaces struct {
	semantic map[string]bool
}

func (twoWorkspaces) Authenticate(_ context.Context, token string) (store.ActiveGatewayKey, error) {
	switch token {
	case testKey:
		return store.ActiveGatewayKey{ID: "k1", WorkspaceID: "ws1", OwnerVerified: true}, nil
	case otherKey:
		return store.ActiveGatewayKey{ID: "k3", WorkspaceID: "ws2", OwnerVerified: true}, nil
	}
	return store.ActiveGatewayKey{}, auth.ErrInvalidGatewayKey
}

func (w twoWorkspaces) ResolveRoute(ctx context.Context, workspaceID, alias string) (*store.ResolvedRoute, error) {
	route, err := fakeRoutes{}.ResolveRoute(ctx, "ws1", alias)
	if err != nil {
		return nil, err
	}
	route.Cache, route.Version = store.CacheAlways, 1
	route.Settings.SemanticCache = w.semantic[workspaceID]
	return route, nil
}

type vectorCache struct {
	mu      sync.Mutex
	entries map[string][]vectorEntry
	lookups int
}

type vectorEntry struct {
	ctxHash cache.ContextHash
	vector  []float32
	entry   cache.Entry
}

func (c *vectorCache) key(workspaceID string, v cache.Vector) string {
	return workspaceID + ":" + v.Family
}

func (c *vectorCache) Lookup(_ context.Context, workspaceID string, fam cache.Family, ctxHash cache.ContextHash, v cache.Vector) (*cache.Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups++
	var best *cache.Entry
	bestScore := fam.Threshold
	for _, e := range c.entries[c.key(workspaceID, v)] {
		var dot float64
		for i := range e.vector {
			dot += float64(e.vector[i]) * float64(v.Values[i])
		}
		if e.ctxHash == ctxHash && dot >= bestScore && !math.IsNaN(dot) {
			e := e.entry
			best, bestScore = &e, dot
		}
	}
	return best, nil
}

func (c *vectorCache) Put(_ context.Context, workspaceID string, _ cache.Family, ctxHash cache.ContextHash, v cache.Vector, e cache.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[c.key(workspaceID, v)] = append(c.entries[c.key(workspaceID, v)], vectorEntry{ctxHash, v.Values, e})
	return nil
}

func (c *vectorCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, es := range c.entries {
		n += len(es)
	}
	return n
}

func (c *vectorCache) lookupCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookups
}

type semanticFixture struct {
	cacheFixture
	vectors *vectorCache
}

func semanticServer(t *testing.T, semantic map[string]bool) semanticFixture {
	t.Helper()
	routes := twoWorkspaces{semantic: semantic}
	vectors := &vectorCache{entries: map[string][]vectorEntry{}}
	f := semanticFixture{vectors: vectors}
	f.cache = &memoryCache{entries: map[string]cache.Entry{}}
	f.usage, f.calls = &fakeUsage{}, &calls{}
	d := deps(f.calls, time.Minute)
	d.Keys, d.Routes, d.Usage, d.Cache = routes, routes, f.usage, f.cache
	d.Semantic = vectors
	d.Embeddings = cache.NewEmbeddings(map[provider.Vendor]provider.Embedder{provider.VendorMock: &mock.Provider{}}, time.Second)
	f.srv = httptest.NewServer(http.StripPrefix("/v1", Routes(d)))
	t.Cleanup(f.srv.Close)
	return f
}

func ask(t *testing.T, srv *httptest.Server, token, system, question string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "echo",
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": question},
		},
	})
	resp := postAs(t, srv, "Bearer "+token, string(body))
	text := completionText(t, resp.Body)
	return resp, text
}

func (f semanticFixture) waitStored(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); f.vectors.size() != n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the semantic cache holds %d entries, want %d", f.vectors.size(), n)
		}
	}
}

const (
	question  = "please explain how the capital city of france got chosen"
	reworded  = "please explain how the capital city of france was chosen"
	unrelated = "write a haiku about autumn rain"
)

func TestARewordedQuestionIsAnsweredFromTheSemanticCache(t *testing.T) {
	f := semanticServer(t, map[string]bool{"ws1": true})

	first, firstText := ask(t, f.srv, testKey, "be brief", question)
	wantCache(t, first, "MISS")
	f.waitStored(t, 1)

	second, secondText := ask(t, f.srv, testKey, "be brief", reworded)
	wantCache(t, second, "HIT")
	wantTrail(t, second, "0", "")
	if secondText != firstText {
		t.Errorf("the hit answered %q, want the stored %q", secondText, firstText)
	}
	if n := f.upstreamCalls(); n != 1 {
		t.Errorf("the upstream was called %d times, want once", n)
	}

	events := recordedEvents(t, f.usage, 2)
	hit := events[1]
	if !hit.Cached || hit.PromptTokens != 0 || hit.CompletionTokens != 0 || hit.Attempts != 0 {
		t.Errorf("the hit was recorded as %+v, want cached with no tokens and no attempts", hit)
	}
	if got := cacheCounts(t)["semantic_hit"]; got < 1 {
		t.Errorf("gateway.cache counted %d semantic hits, want at least one", got)
	}
}

func TestASemanticMissIsNotServedOtherContextsOrOtherQuestions(t *testing.T) {
	f := semanticServer(t, map[string]bool{"ws1": true})
	ask(t, f.srv, testKey, "be brief", question)
	f.waitStored(t, 1)

	if resp, _ := ask(t, f.srv, testKey, "answer in french", reworded); resp.Header.Get("X-Cache") != "MISS" {
		t.Errorf("a different system prompt: X-Cache = %q, want MISS", resp.Header.Get("X-Cache"))
	}
	if resp, _ := ask(t, f.srv, testKey, "be brief", unrelated); resp.Header.Get("X-Cache") != "MISS" {
		t.Errorf("an unrelated question: X-Cache = %q, want MISS", resp.Header.Get("X-Cache"))
	}
	if n := f.upstreamCalls(); n != 3 {
		t.Errorf("the upstream was called %d times, want 3", n)
	}
}

func TestAWorkspaceThatDidNotOptInNeverTriesTheSemanticCache(t *testing.T) {
	f := semanticServer(t, map[string]bool{"ws1": false})

	first, _ := ask(t, f.srv, testKey, "be brief", question)
	wantCache(t, first, "MISS")
	second, _ := ask(t, f.srv, testKey, "be brief", reworded)
	wantCache(t, second, "MISS")
	io.Copy(io.Discard, second.Body)
	if n := f.vectors.lookupCount(); n != 0 {
		t.Errorf("%d semantic lookups, want none", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.vectors.size(); n != 0 {
		t.Errorf("%d semantic entries stored, want none", n)
	}
}

func TestASemanticAnswerIsNeverServedToAnotherWorkspace(t *testing.T) {
	f := semanticServer(t, map[string]bool{"ws1": true, "ws2": true})
	ask(t, f.srv, testKey, "be brief", question)
	f.waitStored(t, 1)

	resp, _ := ask(t, f.srv, otherKey, "be brief", reworded)
	wantCache(t, resp, "MISS")
}
