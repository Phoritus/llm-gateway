package api

// Email-verification enforcement. This is the first slice off
// integration_test.go, which had grown to the 1000-line limit covering auth,
// isolation and rate limits in one file; the rest of that split is still to
// come. The shared fixtures -- newTestEnv, client, the capturing mailer -- live
// there and are available to every test in the package.

import (
	"net/http"
	"testing"
)

// An unverified account can sign in and reach the account-hygiene endpoints,
// which is the deliberate exemption from the verification gate: a user who
// cannot yet use the service must still be able to see why, secure the account
// and sign out.
//
// The gate itself is covered by TestRequireVerifiedEmail in internal/auth,
// because the route group it guards holds no routes until build-order step 3
// adds gateway keys. This test exists so the exemption is not narrowed by
// accident in the meantime -- locking an unverified user out of
// change-password would be a plausible-looking tightening that strands anyone
// whose address never confirms.
func TestUnverifiedAccountKeepsAccountHygieneEndpoints(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	const email = "unverified@example.com"
	env.signup(c, email)

	if res := env.login(c, email, testPassword); res.status != http.StatusOK {
		t.Fatalf("login while unverified: status = %d, body = %s", res.status, res.raw)
	}

	me := c.do(http.MethodGet, "/api/me", nil)
	if me.status != http.StatusOK {
		t.Fatalf("/api/me while unverified: status = %d, body = %s", me.status, me.raw)
	}
	user, _ := me.body["user"].(map[string]any)
	if verified, _ := user["email_verified"].(bool); verified {
		t.Error("email_verified = true for an account that never confirmed")
	}

	if res := c.do(http.MethodPost, "/api/auth/change-password", map[string]any{
		"current_password": testPassword,
		"new_password":     testNewPassword,
	}); res.status != http.StatusOK {
		t.Fatalf("change-password while unverified: status = %d, body = %s", res.status, res.raw)
	}

	// The change revoked every other session and left this one alive, so
	// logout-all is still reachable too.
	if res := c.do(http.MethodPost, "/api/auth/logout-all", nil); res.status != http.StatusOK {
		t.Fatalf("logout-all while unverified: status = %d, body = %s", res.status, res.raw)
	}
}
