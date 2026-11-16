package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm-gateway/internal/cache"
	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/quota"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
)

// echoIsFree is a catalog in which the mock's echo model has a free tier of
// freeRPM requests a minute and freeTPM tokens a minute, and nothing else does.
type echoIsFree struct{}

const (
	freeRPM = 2
	freeTPM = 1000
)

func (echoIsFree) FreeQuotas(context.Context) ([]store.FreeQuota, error) {
	rpm, tpm := freeRPM, freeTPM
	return []store.FreeQuota{{Vendor: "mock", Model: "echo", RPM: &rpm, TPM: &tpm}}, nil
}

type freeFixture struct {
	srv     *httptest.Server
	limiter *ratelimit.Local
	cache   *memoryCache
}

func freeServer(t *testing.T, freeFirst bool) freeFixture {
	t.Helper()
	var catalog quota.Catalog
	if err := catalog.Load(context.Background(), echoIsFree{}); err != nil {
		t.Fatal(err)
	}
	f := freeFixture{limiter: ratelimit.NewLocal(), cache: &memoryCache{entries: map[string]cache.Entry{}}}
	d := deps(&mock.Provider{Delay: time.Millisecond}, time.Minute)
	d.Routes = &cachingRoutes{mode: store.CacheAuto, freeFirst: freeFirst}
	d.Quota = quota.New(&catalog, f.limiter, nil)
	d.Cache = f.cache
	f.srv = httptest.NewServer(http.StripPrefix("/v1", Routes(d)))
	t.Cleanup(f.srv.Close)
	return f
}

// The free tier's buckets for the echo target, as quota names them.
var (
	freeRequests = ratelimit.Bucket{Scope: "free:rpm", ID: "c-echo:echo", Rule: ratelimit.PerMinute(freeRPM)}
	freeTokens   = ratelimit.Bucket{Scope: "free:tpm", ID: "c-echo:echo", Rule: ratelimit.PerMinute(freeTPM)}
)

// waitForFree waits for the count, which runs after the response. The buckets
// refill as the test runs, so a level is held to the whole number it was
// spent down to.
func waitForFree(t *testing.T, l *ratelimit.Local, b ratelimit.Bucket, want float64) {
	t.Helper()
	var got float64
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		levels, err := l.Peek(context.Background(), []ratelimit.Bucket{b})
		if err != nil {
			t.Fatal(err)
		}
		if got = levels[0]; got >= want && got < want+b.Rule.Refill*3 {
			return
		}
	}
	t.Fatalf("bucket %s holds %v, want %v", b.Scope, got, want)
}

func TestAFreeFirstRouteTriesItsFreeTargetFirst(t *testing.T) {
	f := freeServer(t, true)
	resp := post(t, f.srv, chatBody("fail,echo", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantTrail(t, resp, "1", "mock/echo=ok")
}

func TestARouteThatIsNotFreeFirstKeepsItsOrder(t *testing.T) {
	f := freeServer(t, false)
	resp := post(t, f.srv, chatBody("fail,echo", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantTrail(t, resp, "2", "mock/fail=upstream_error, mock/echo=ok")
}

func TestAFreeTargetOutOfQuotaIsTriedAfterTheRest(t *testing.T) {
	f := freeServer(t, true)
	for i := range freeRPM {
		drain(post(t, f.srv, chatBody("slow,echo", false)))
		waitForFree(t, f.limiter, freeRequests, float64(freeRPM-i-1))
	}

	resp := post(t, f.srv, chatBody("slow,echo", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	wantTrail(t, resp, "1", "mock/slow=ok")
}

func TestAnAnswerSpendsItsTargetsFreeQuotaOnAnyRoute(t *testing.T) {
	f := freeServer(t, false)
	drain(post(t, f.srv, chatBody("echo", false)))

	waitForFree(t, f.limiter, freeRequests, freeRPM-1)
	waitForFree(t, f.limiter, freeTokens, freeTPM-echoTokens)
}

func TestACacheHitSpendsNoFreeQuota(t *testing.T) {
	f := freeServer(t, true)
	body := deterministic("echo", false, nil)
	drain(post(t, f.srv, body))
	f.cache.waitStored(t, 1)
	waitForFree(t, f.limiter, freeRequests, freeRPM-1)

	wantCache(t, post(t, f.srv, body), "HIT")
	time.Sleep(50 * time.Millisecond)
	waitForFree(t, f.limiter, freeRequests, freeRPM-1)
	waitForFree(t, f.limiter, freeTokens, freeTPM-echoTokens)
}
