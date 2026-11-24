package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/cache"
	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/store"
)

type handoffRoutes struct{ mode store.HandoffMode }

func (h handoffRoutes) ResolveRoute(ctx context.Context, workspaceID, alias string) (*store.ResolvedRoute, error) {
	route, err := fakeRoutes{}.ResolveRoute(ctx, workspaceID, alias)
	if err != nil {
		return nil, err
	}
	route.ContextHandoff, route.Cache = h.mode, store.CacheAlways
	return route, nil
}

type handoffFixture struct {
	srv   *httptest.Server
	cache *memoryCache
	usage *fakeUsage
}

func handoffServer(t *testing.T, mode store.HandoffMode, enabled bool) handoffFixture {
	t.Helper()
	f := handoffFixture{cache: &memoryCache{entries: map[string]cache.Entry{}}, usage: &fakeUsage{}}
	d := deps(&mock.Provider{}, time.Minute)
	d.Routes, d.Cache, d.Usage, d.ContextHandoff = handoffRoutes{mode}, f.cache, f.usage, enabled
	f.srv = httptest.NewServer(http.StripPrefix("/v1", Routes(d)))
	t.Cleanup(f.srv.Close)
	return f
}

func longConversation(model string) string {
	msgs := []map[string]string{{"role": "system", "content": "be brief"}}
	for range 20 {
		msgs = append(msgs,
			map[string]string{"role": "user", "content": strings.Repeat("earlier question ", 30)},
			map[string]string{"role": "assistant", "content": strings.Repeat("earlier answer ", 30)})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": "one two three four"})
	b, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "temperature": 0})
	return string(b)
}

func TestAConversationOverASmallWindowIsTrimmedWhenTheBigTargetIsLimited(t *testing.T) {
	const route = "rate-limited,window-2000"
	for _, tc := range []struct {
		name    string
		mode    store.HandoffMode
		enabled bool
	}{
		{"reorder", store.HandoffReorder, true},
		{"trim behind the switch", store.HandoffTrim, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := handoffServer(t, tc.mode, tc.enabled)
			resp := post(t, f.srv, longConversation(route))
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429: the limited target is the only one it fits", resp.StatusCode)
			}
			if h := resp.Header.Get("X-Context-Handoff"); h != "" {
				t.Errorf("X-Context-Handoff = %q, want none", h)
			}
		})
	}

	f := handoffServer(t, store.HandoffTrim, true)
	resp := post(t, f.srv, longConversation(route))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if got := completionText(t, resp.Body); got != "one two three four" {
		t.Errorf("completion = %q, want the last question answered", got)
	}
	wantTrail(t, resp, "3", "mock/rate-limited=rate_limited, mock/window-2000=context_too_long, mock/window-2000=ok+trimmed")
	if h := resp.Header.Get("X-Context-Handoff"); !strings.HasPrefix(h, "trimmed; omitted=") {
		t.Errorf("X-Context-Handoff = %q, want trimmed with its counts", h)
	}
	f.cache.staysEmpty(t)
	if e := f.usage.recorded(); len(e) != 1 || e[0].ContextHandoff != store.HandoffTrimmed {
		t.Errorf("usage = %+v, want one event recorded as trimmed", e)
	}
}

func TestAConversationNoTargetCanHoldIsA400StatingTheWindow(t *testing.T) {
	f := handoffServer(t, store.HandoffReorder, true)
	resp := post(t, f.srv, longConversation("window-2000"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body errorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "context_length_exceeded" ||
		!strings.HasPrefix(body.Error.Message, "This model's maximum context length is 2000 tokens. However, your messages resulted in ") {
		t.Errorf("error = %+v", body.Error)
	}
}
