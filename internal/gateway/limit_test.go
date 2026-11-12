package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
)

// echoTokens is the usage the mock reports for chatBody: 18 characters of
// prompt and the same 18 echoed back, at four characters a token.
const echoTokens = 10

// tight refills so little during a test that a bucket's level is what the
// test spent from it.
func tight(burst float64) ratelimit.Rule {
	return ratelimit.Rule{Burst: burst, Refill: 0.001, TTL: time.Minute}
}

// limitedServer is newServer with its limits adjusted, and the limiter handed
// back so a test can read a bucket.
func limitedServer(t *testing.T, p provider.Provider, adjust func(*Deps)) (*httptest.Server, *ratelimit.Local) {
	t.Helper()
	d := deps(p, time.Minute)
	limiter := ratelimit.NewLocal()
	d.Limiter = limiter
	adjust(&d)
	srv := httptest.NewServer(http.StripPrefix("/v1", Routes(d)))
	t.Cleanup(srv.Close)
	return srv, limiter
}

// level reads a bucket by taking nothing from it.
func level(t *testing.T, l *ratelimit.Local, scope, id string, rule ratelimit.Rule) float64 {
	t.Helper()
	res, err := l.Take(context.Background(), []ratelimit.Bucket{{Scope: scope, ID: id, Rule: rule}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Remaining[0]
}

// waitForLevel waits for the reconcile, which runs after the response.
func waitForLevel(t *testing.T, l *ratelimit.Local, scope, id string, rule ratelimit.Rule, want float64) {
	t.Helper()
	var got float64
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if got = level(t, l, scope, id, rule); got >= want-0.5 && got <= want+0.5 {
			return
		}
	}
	t.Fatalf("bucket %s holds %v, want %v", scope, got, want)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestKeyPastItsRequestLimitIsRefused(t *testing.T) {
	srv, _ := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Limits.KeyRequests = tight(2) })

	for i := range 2 {
		if resp := post(t, srv, chatBody("mock/echo", false)); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, resp.StatusCode)
		}
	}

	resp := post(t, srv, chatBody("mock/echo", false))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.StatusCode, body)
	}
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Type != typeRateLimit || e.Error.Code != "rate_limit_exceeded" {
		t.Errorf("body = %s, want the OpenAI envelope with rate_limit_error", body)
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || seconds < 1 {
		t.Errorf("Retry-After = %q, want whole seconds of at least 1", resp.Header.Get("Retry-After"))
	}
	if got := resp.Header.Get("X-RateLimit-Remaining-Requests"); got != "0" {
		t.Errorf("X-RateLimit-Remaining-Requests = %q on a refusal, want 0", got)
	}
}

func TestRefusalByTheWorkspaceSpendsNothingFromTheKey(t *testing.T) {
	keyRequests, keyTokens := tight(5), tight(1000)
	srv, limiter := limitedServer(t, &mock.Provider{}, func(d *Deps) {
		d.Limits.KeyRequests, d.Limits.KeyTokens = keyRequests, keyTokens
		d.Limits.WorkspaceRequests = tight(1)
	})

	drain(post(t, srv, chatBody("mock/echo", false)))
	waitForLevel(t, limiter, scopeKeyTokens, "k1", keyTokens, 1000-echoTokens)

	if resp := post(t, srv, chatBody("mock/echo", false)); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the workspace's limit to refuse", resp.StatusCode)
	}
	if got := level(t, limiter, scopeKeyRequests, "k1", keyRequests); got < 3.9 {
		t.Errorf("the key holds %v requests, want 4: the refused request was charged to it", got)
	}
	if got := level(t, limiter, scopeKeyTokens, "k1", keyTokens); got < 1000-echoTokens-0.5 {
		t.Errorf("the key holds %v tokens, want %d: the refused request reserved from it", got, 1000-echoTokens)
	}
}

func TestAdmittedResponsesCarryWhatIsLeft(t *testing.T) {
	srv, _ := limitedServer(t, &mock.Provider{}, func(d *Deps) {
		d.Limits.KeyRequests = tight(5)
		d.Limits.WorkspaceTokens = tight(1000)
	})
	body := chatBody("mock/echo", false)

	resp := post(t, srv, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining-Requests"); got != "4" {
		t.Errorf("X-RateLimit-Remaining-Requests = %q, want 4", got)
	}
	// The body is estimated at four bytes a token and reserved before the call.
	want := strconv.Itoa(1000 - (len(body)+3)/4)
	if got := resp.Header.Get("X-RateLimit-Remaining-Tokens"); got != want {
		t.Errorf("X-RateLimit-Remaining-Tokens = %q, want %s", got, want)
	}
}

// A request estimated above the whole token allowance would be refused
// forever if the estimate were held against it unclamped. It is let in once,
// and what it really cost is then owed.
func TestRequestLargerThanTheTokenLimitIsAdmittedOnceThenOwed(t *testing.T) {
	keyTokens := tight(8)
	srv, limiter := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Limits.KeyTokens = keyTokens })

	if resp := post(t, srv, chatBody("mock/echo", false)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the first request admitted", resp.StatusCode)
	}
	waitForLevel(t, limiter, scopeKeyTokens, "k1", keyTokens, 8-echoTokens)

	if resp := post(t, srv, chatBody("mock/echo", false)); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 while the bucket is in debt", resp.StatusCode)
	}
}

func TestStreamHidesUsageTheClientDidNotAskFor(t *testing.T) {
	keyTokens := tight(1000)
	srv, limiter := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Limits.KeyTokens = keyTokens })

	resp := post(t, srv, chatBody("mock/echo", true))
	ev := events(t, resp.Body)
	if len(ev) < 2 || ev[len(ev)-1] != "[DONE]" {
		t.Fatalf("events = %v", ev)
	}
	for _, e := range ev[:len(ev)-1] {
		var c provider.ChatChunk
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			t.Fatalf("chunk %q: %v", e, err)
		}
		if strings.Contains(e, `"usage"`) || len(c.Choices) == 0 {
			t.Errorf("the client was sent %s, which exists only because the gateway asked for usage", e)
		}
	}

	// Unseen by the client, the usage still corrected the reservation.
	waitForLevel(t, limiter, scopeKeyTokens, "k1", keyTokens, 1000-echoTokens)
}

func TestStreamKeepsUsageTheClientAskedFor(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	body := strings.Replace(chatBody("mock/echo", true), "{", `{"stream_options":{"include_usage":true},`, 1)

	ev := events(t, post(t, srv, body).Body)
	if len(ev) < 2 {
		t.Fatalf("events = %v", ev)
	}
	var last provider.ChatChunk
	if err := json.Unmarshal([]byte(ev[len(ev)-2]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Usage == nil || last.Usage.TotalTokens != echoTokens || len(last.Choices) != 0 {
		t.Fatalf("last chunk = %s, want the usage chunk", ev[len(ev)-2])
	}
}

func TestUpstreamFailureBeforeTheFirstByteRefundsTheTokens(t *testing.T) {
	keyRequests, keyTokens, workspaceTokens := tight(5), tight(1000), tight(2000)
	srv, limiter := limitedServer(t, &mock.Provider{}, func(d *Deps) {
		d.Limits.KeyRequests, d.Limits.KeyTokens, d.Limits.WorkspaceTokens = keyRequests, keyTokens, workspaceTokens
	})

	if resp := post(t, srv, chatBody("mock/fail", true)); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	waitForLevel(t, limiter, scopeKeyTokens, "k1", keyTokens, 1000)
	waitForLevel(t, limiter, scopeWorkspaceTokens, "ws1", workspaceTokens, 2000)
	// The request itself happened, whatever became of it.
	if got := level(t, limiter, scopeKeyRequests, "k1", keyRequests); got > 4.1 {
		t.Errorf("the key holds %v requests, want 4: the request count was refunded", got)
	}
}

// holding reports when its first generation starts and when that one's
// context ends. Later generations pass through unwatched.
type holding struct {
	mock.Provider
	first          sync.Once
	started, ended chan struct{}
}

func (h *holding) ChatStream(ctx context.Context, req *provider.ChatRequest, c provider.Credential) (provider.Stream, error) {
	h.first.Do(func() {
		close(h.started)
		go func() { <-ctx.Done(); close(h.ended) }()
	})
	return h.Provider.ChatStream(ctx, req, c)
}

func TestWorkspacePastItsConcurrencyCapIsRefused(t *testing.T) {
	p := &holding{Provider: mock.Provider{Delay: time.Hour}, started: make(chan struct{}), ended: make(chan struct{})}
	srv, _ := limitedServer(t, p, func(d *Deps) { d.Limits.WorkspaceConcurrency = 1 })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(chatBody("mock/slow", true)))
	held.Header.Set("Authorization", "Bearer "+testKey)
	go func() {
		if resp, err := http.DefaultClient.Do(held); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the held generation never started")
	}

	resp := post(t, srv, chatBody("mock/echo", false))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || errorCode(t, body) != "too_many_concurrent_requests" {
		t.Fatalf("%d %s, want 429 too_many_concurrent_requests", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}

	cancel()
	<-p.ended
	// The slot comes back when the held handler returns, a moment after its
	// context ends. The refusals in between are the cap still doing its job.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		resp := post(t, srv, chatBody("mock/break", false))
		drain(resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was not freed after the held request ended")
		}
	}
}

// countingKeys counts the lookups that reach the key store.
type countingKeys struct {
	fakeKeys
	lookups atomic.Int64
}

func (c *countingKeys) Authenticate(ctx context.Context, token string) (store.ActiveGatewayKey, error) {
	c.lookups.Add(1)
	return c.fakeKeys.Authenticate(ctx, token)
}

func TestAddressPastItsFailedAuthenticationsIsRefusedWithoutALookup(t *testing.T) {
	keys := &countingKeys{}
	srv, _ := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Keys = keys })
	allowed := int(authFailPerIP.Burst)

	for i := range allowed {
		if resp := postAs(t, srv, "Bearer gwk_nope", chatBody("mock/echo", false)); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i, resp.StatusCode)
		}
	}

	resp := postAs(t, srv, "Bearer gwk_nope", chatBody("mock/echo", false))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q; want 429 past the limit", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	lookups := keys.lookups.Load()

	// The address is now turned away at the door, a valid key included: the
	// gateway cannot tell the keys apart without the lookup it is avoiding.
	for _, authorization := range []string{"Bearer gwk_another", "Bearer " + testKey} {
		if resp := postAs(t, srv, authorization, chatBody("mock/echo", false)); resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s: status %d, want 429 while the address is blocked", authorization, resp.StatusCode)
		}
	}
	if got := keys.lookups.Load(); got != lookups {
		t.Errorf("the key store was asked %d more times by a blocked address", got-lookups)
	}
}

func TestUnverifiedOwnerIsNotAFailedAuthentication(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	for i := range int(authFailPerIP.Burst) + 5 {
		if resp := postAs(t, srv, "Bearer "+unverifiedKey, chatBody("mock/echo", false)); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("request %d: status %d, want 403 every time", i, resp.StatusCode)
		}
	}
}
