// What protects the service from its callers rather than what it does for
// them: origin checks, health probes, and per-address and per-session rate
// limits, including the trusted-proxy rules that decide whose address counts.

package api

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
)

// State-changing requests from an unlisted origin must be refused, which is
// what protects the endpoints that have no session to bind a CSRF token to.
func TestForeignOriginIsRefused(t *testing.T) {
	env := newTestEnv(t)

	c := env.newClient()
	c.origin = "https://evil.example"

	res := c.do(http.MethodPost, "/api/auth/login", map[string]any{
		"email":    "someone@example.com",
		"password": testPassword,
	})
	if res.status != http.StatusForbidden || res.errorCode() != "origin_not_allowed" {
		t.Fatalf("login from a foreign origin: status = %d, code = %q, want 403 origin_not_allowed",
			res.status, res.errorCode())
	}

	// CORS headers must not be granted to it either.
	if got := res.header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty for a foreign origin", got)
	}
}

// An allowed origin spelled differently from the configured entry must be
// honoured by both readers of the list: the same-origin guard, which decides
// whether the request runs at all, and the CORS headers, which decide whether
// the browser lets the dashboard read the answer. Mixed case with a trailing
// slash is a spelling only config.Origins.normalizeOrigin accepts, so a reader
// that grew its own matching rule again fails here rather than in a deployment
// where one of the two silently stops agreeing.
func TestAllowedOriginIsMatchedByBothReaders(t *testing.T) {
	env := newTestEnv(t)

	c := env.newClient()
	c.origin = "HTTP://LOCALHOST:3000/"

	// A login for an address that does not exist: the point is that the
	// request reached the handler at all rather than being refused as foreign.
	res := c.do(http.MethodPost, "/api/auth/login", map[string]any{
		"email":    "nobody@example.com",
		"password": testPassword,
	})
	if res.errorCode() == "origin_not_allowed" {
		t.Fatalf("login from an allowed origin spelled %q was refused as foreign", c.origin)
	}
	if res.status != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401 for an unknown address", res.status)
	}
	// Echoed exactly as sent, which is what a browser compares against.
	if got := res.header.Get("Access-Control-Allow-Origin"); got != c.origin {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, c.origin)
	}
}

func TestHealthEndpoints(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	if res := c.do(http.MethodGet, "/healthz", nil); res.status != http.StatusOK {
		t.Fatalf("/healthz: status = %d, want 200", res.status)
	}
	res := c.do(http.MethodGet, "/readyz", nil)
	if res.status != http.StatusOK {
		t.Fatalf("/readyz: status = %d, body = %s, want 200", res.status, res.raw)
	}
	checks, _ := res.body["checks"].(map[string]any)
	for _, dep := range []string{"postgres", "redis"} {
		if checks[dep] != "ok" {
			t.Errorf("readiness check %s = %v, want ok", dep, checks[dep])
		}
	}
}

// Every authenticated route is metered, including the ones that only read.
//
// The ceiling is applied to the whole authenticated group rather than inside
// each handler, so this test is really asserting that arrangement: a reader
// like /api/me is covered without anyone having remembered to cover it. Before
// the group middleware existed, /api/me and /auth/logout-all were unmetered,
// and a single valid session could spend the database's time without limit.
func TestAuthenticatedRequestsAreMetered(t *testing.T) {
	env := newTestEnv(t)
	c := env.signupAndLogin("metered@example.com")

	// apiPerUser allows 120 a minute. The loop runs past that and stops at the
	// first refusal; its bound only keeps a regression from hanging the test.
	var limited response
	for i := range 200 {
		res := c.do(http.MethodGet, "/api/me", nil)
		if res.status == http.StatusTooManyRequests {
			limited = res
			break
		}
		if res.status != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, res.status, res.raw)
		}
	}
	if limited.status != http.StatusTooManyRequests {
		t.Fatal("200 reads of /api/me were all allowed: the authenticated ceiling is not applied")
	}
	if limited.errorCode() != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want rate_limit_exceeded", limited.errorCode())
	}
	if limited.header.Get("Retry-After") == "" {
		t.Fatal("the 429 carried no Retry-After header")
	}

	// A second account is unaffected, because the bucket is keyed per user and
	// not per address -- every test here calls from 127.0.0.1.
	other := env.signupAndLogin("unmetered@example.com")
	if res := other.do(http.MethodGet, "/api/me", nil); res.status != http.StatusOK {
		t.Fatalf("another user after the first was limited: status = %d, body = %s",
			res.status, res.raw)
	}
}

// Rate limiting has to actually refuse, and say when to come back.
func TestLoginRateLimit(t *testing.T) {
	env := newTestEnv(t)
	const email = "throttled@example.com"
	env.signupAndLogin(email)

	c := env.newClient()
	var limited response
	// The per-address limit is 10 a minute, so this is reached well inside the
	// loop; the loop bound only keeps a regression from hanging the test.
	for range 40 {
		res := env.login(c, email, "the-wrong-passphrase")
		if res.status == http.StatusTooManyRequests {
			limited = res
			break
		}
		if res.status != http.StatusUnauthorized {
			t.Fatalf("unexpected status %d: %s", res.status, res.raw)
		}
	}

	if limited.status != http.StatusTooManyRequests {
		t.Fatal("repeated failed logins were never rate limited")
	}
	if limited.errorCode() != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want rate_limit_exceeded", limited.errorCode())
	}
	if limited.header.Get("Retry-After") == "" {
		t.Fatal("a 429 was returned with no Retry-After header")
	}
}

// A per-address limit that a client can re-key by setting a header is not a
// limit. X-Forwarded-For is believed only when the peer is a trusted proxy,
// and the test fixture trusts none, so every attempt below must land in the
// one bucket belonging to 127.0.0.1.
func TestLoginRateLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	env := newTestEnv(t)
	const email = "spoofed@example.com"
	env.signupAndLogin(email)

	c := env.newClient()
	var (
		limited  response
		attempts int
	)
	for i := range 40 {
		// A fresh address every attempt: before the fix each one of these
		// minted its own bucket and the loop never saw a 429.
		c.forwardedFor = netip.AddrFrom4([4]byte{203, 0, 113, byte(i)}).String()

		attempts++
		res := env.login(c, email, "the-wrong-passphrase")
		if res.status == http.StatusTooManyRequests {
			limited = res
			break
		}
		if res.status != http.StatusUnauthorized {
			t.Fatalf("unexpected status %d: %s", res.status, res.raw)
		}
	}

	if limited.status != http.StatusTooManyRequests {
		t.Fatal("a rotating X-Forwarded-For escaped the per-address login limit")
	}
	if limited.errorCode() != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want rate_limit_exceeded", limited.errorCode())
	}
	// The count matters, not just that a 429 arrived eventually. login:ip
	// holds 10 a minute and login:email 20 an hour, so a bypassed address
	// bucket still trips the email one around the 21st attempt — which would
	// let this test pass while the bug was live. The address bucket must be
	// what stops it.
	if attempts > 11 {
		t.Fatalf("the 429 arrived on attempt %d: the per-address bucket was bypassed "+
			"and the per-email bucket is what refused the request", attempts)
	}
}

// The audit column records where the request actually came from, so a client
// cannot write a chosen address into another user's session history.
func TestSessionRecordsThePeerNotTheForwardedHeader(t *testing.T) {
	env := newTestEnv(t)
	const email = "audited@example.com"

	c := env.signupAndLogin(email)
	// A second login, this time claiming to be somewhere else. It is the most
	// recent session, so it is the row read back below.
	c.forwardedFor = "203.0.113.9"
	if res := env.login(c, email, testPassword); res.status != http.StatusOK {
		t.Fatalf("login: status = %d, body = %s", res.status, res.raw)
	}

	var ip *netip.Addr
	err := env.store.Pool().QueryRow(context.Background(),
		`SELECT ip FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&ip)
	if err != nil {
		t.Fatalf("select session ip: %v", err)
	}
	if ip == nil {
		t.Fatal("the session recorded no address at all")
	}
	if ip.String() != "127.0.0.1" {
		t.Fatalf("session ip = %s, want the peer 127.0.0.1", ip)
	}
}
