package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/mock"
)

// The routes here are chains: fakeRoutes makes one target of each
// comma-separated model in the alias.

func wantTrail(t *testing.T, resp *http.Response, attempts, trail string) {
	t.Helper()
	if got := resp.Header.Get("X-Fallback-Attempts"); got != attempts {
		t.Errorf("X-Fallback-Attempts = %q, want %q", got, attempts)
	}
	if got := resp.Header.Get("X-Fallback-Trail"); got != trail {
		t.Errorf("X-Fallback-Trail = %q, want %q", got, trail)
	}
}

func completionText(t *testing.T, body io.Reader) string {
	t.Helper()
	var r provider.ChatResponse
	if err := json.NewDecoder(body).Decode(&r); err != nil || len(r.Choices) != 1 {
		t.Fatalf("not a completion: %v %+v", err, r)
	}
	return r.Choices[0].Message.Content
}

func TestARateLimitedTargetFailsOverToTheNext(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody("rate-limited,echo", false))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the second target's answer", resp.StatusCode)
	}
	if got := completionText(t, resp.Body); got != "one two three four" {
		t.Errorf("completion = %q, want the echo", got)
	}
	wantTrail(t, resp, "2", "mock/rate-limited=rate_limited, mock/echo=ok")
}

func TestATargetSilentPastItsFirstByteTimeoutFailsOver(t *testing.T) {
	srv := newServer(t, &mock.Provider{Delay: time.Hour}, time.Minute)
	resp := post(t, srv, chatBody("slow@50ms,echo", false))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the second target's answer", resp.StatusCode)
	}
	if got := completionText(t, resp.Body); got != "one two three four" {
		t.Errorf("completion = %q, want the echo", got)
	}
	wantTrail(t, resp, "2", "mock/slow=timeout, mock/echo=ok")
}

func TestTheFirstByteTimeoutStopsAtTheFirstChunk(t *testing.T) {
	const perChunk, firstByteWait = 40 * time.Millisecond, 60 * time.Millisecond
	wholeAnswer := 4 * perChunk
	if wholeAnswer <= firstByteWait || perChunk >= firstByteWait {
		t.Fatal("the answer must outlast the wait while its first chunk does not")
	}
	srv := newServer(t, &mock.Provider{Delay: perChunk}, time.Minute)
	resp := post(t, srv, chatBody("slow@"+firstByteWait.String(), false))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the slow target's answer", resp.StatusCode)
	}
	if got := completionText(t, resp.Body); got != "one two three four" {
		t.Errorf("completion = %q, want every word", got)
	}
	wantTrail(t, resp, "1", "mock/slow=ok")
}

func TestAnExhaustedRouteIsAnsweredByWhatItsTargetsSaid(t *testing.T) {
	cases := []struct {
		route      string
		status     int
		code       string
		retryAfter string
		attempts   string
		trail      string
	}{
		{"fail,fail", http.StatusBadGateway, "upstream_error", "",
			"2", "mock/fail=upstream_error, mock/fail=upstream_error"},
		{"rate-limited,context-too-long", http.StatusTooManyRequests, "upstream_rate_limited", "90",
			"2", "mock/rate-limited=rate_limited, mock/context-too-long=context_too_long"},
		// The second target is the same model, so the same window: not called.
		{"context-too-long,context-too-long", http.StatusRequestEntityTooLarge, "context_length_exceeded", "",
			"1", "mock/context-too-long=context_too_long, mock/context-too-long=skipped"},
		{"context-too-long,fail", http.StatusBadGateway, "upstream_error", "",
			"2", "mock/context-too-long=context_too_long, mock/fail=upstream_error"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{true, false} {
			srv := newServer(t, &mock.Provider{}, time.Minute)
			resp := post(t, srv, chatBody(tc.route, stream))
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || errorCode(t, body) != tc.code {
				t.Errorf("%s stream=%v: %d %s, want %d %s", tc.route, stream, resp.StatusCode, body, tc.status, tc.code)
			}
			if got := resp.Header.Get("Retry-After"); got != tc.retryAfter {
				t.Errorf("%s: Retry-After = %q, want %q", tc.route, got, tc.retryAfter)
			}
			wantTrail(t, resp, tc.attempts, tc.trail)
		}
	}
}

func TestAFailureAfterTheFirstChunkIsNotFailedOverFrom(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody("break,echo", true))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantTrail(t, resp, "1", "mock/break=ok")
	ev := events(t, resp.Body)
	if len(ev) != mock.BreakAfter+1 {
		t.Fatalf("events = %v, want the broken stream's %d chunks and an error, nothing from the next target", ev, mock.BreakAfter)
	}
	if got := errorCode(t, []byte(ev[len(ev)-1])); got != "upstream_error" {
		t.Errorf("last event code = %q", got)
	}
}

// calls records the model of every generation asked of the mock.
type calls struct {
	mock.Provider
	mu     sync.Mutex
	models []string
}

func (c *calls) ChatStream(ctx context.Context, req *provider.ChatRequest, cred provider.Credential) (provider.Stream, error) {
	c.mu.Lock()
	c.models = append(c.models, req.Model)
	c.mu.Unlock()
	return c.Provider.ChatStream(ctx, req, cred)
}

func TestATargetThatJustFailedIsSkippedByTheNextRequest(t *testing.T) {
	p := &calls{}
	srv := newServer(t, p, time.Minute)

	drain(post(t, srv, chatBody("rate-limited,echo", false)))
	resp := post(t, srv, chatBody("rate-limited,echo", false))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantTrail(t, resp, "1", "mock/rate-limited=skipped, mock/echo=ok")
	if want := []string{"rate-limited", "echo", "echo"}; !slices.Equal(p.models, want) {
		t.Errorf("the mock was asked for %v, want %v: the second request called the rate-limited target again", p.models, want)
	}
}

func TestATargetWithoutAnAdapterIsPassedOver(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody(adapterlessPrefix+"gpt-4o,echo", false))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantTrail(t, resp, "2", "openai/gpt-4o=unavailable, mock/echo=ok")
}

// A model name is the workspace's own text, and a header cannot carry all of
// it.
func TestTheTrailCarriesOnlyPrintableASCIIOfAModelName(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody("modèle\t日本-1", false))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantTrail(t, resp, "1", "mock/modle-1=ok")
}

func TestOnlyTheAnsweringTargetsUsageIsCharged(t *testing.T) {
	keyTokens := tight(1000)
	srv, limiter := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Limits.KeyTokens = keyTokens })

	resp := post(t, srv, chatBody("fail,rate-limited,echo", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var r provider.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Usage == nil {
		t.Fatalf("no usage reported: %v", err)
	}
	waitForLevel(t, limiter, scopeKeyTokens, "k1", keyTokens, 1000-float64(r.Usage.TotalTokens))
}
