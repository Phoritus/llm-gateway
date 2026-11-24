// Behavior of the auth endpoints end to end: signup, login, logout, session
// revocation, password reset and change, and verification. It is its own file
// because these tests all follow one account through its life, unlike the
// tenant-boundary and throttling checks.

package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSignupToLogoutFlow walks the whole happy path and the CSRF gate.
func TestSignupToLogoutFlow(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	const email = "owner@example.com"
	token := env.signup(c, email)

	if body, want := env.mailer.last(t).Body, env.server.URL+"/verify-email?token="; !strings.Contains(body, want) {
		t.Fatalf("verification mail does not link %s...:\n%s", want, body)
	}

	// Before verification the account exists but is unverified, and /api/me
	// is unreachable without a session.
	if res := c.do(http.MethodGet, "/api/me", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("/api/me before login: status = %d, want 401", res.status)
	}

	if res := c.do(http.MethodGet, "/api/auth/verify-email?token="+url.QueryEscape(token), nil); res.status != http.StatusOK {
		t.Fatalf("verify by link: status = %d, body = %s", res.status, res.raw)
	}

	res := env.login(c, email, testPassword)
	if res.status != http.StatusOK {
		t.Fatalf("login: status = %d, body = %s", res.status, res.raw)
	}
	if c.sessionCookie() == "" {
		t.Fatal("login set no session cookie")
	}
	if c.csrfToken() == "" {
		t.Fatal("login set no CSRF cookie")
	}

	me := c.do(http.MethodGet, "/api/me", nil)
	if me.status != http.StatusOK {
		t.Fatalf("/api/me: status = %d, body = %s", me.status, me.raw)
	}
	user, _ := me.body["user"].(map[string]any)
	if verified, _ := user["email_verified"].(bool); !verified {
		t.Fatal("email_verified = false after verification")
	}

	// A state-changing request without the CSRF header must be refused even
	// though the session cookie is present and valid.
	withoutCSRF := *c
	withoutCSRF.noCSRF = true
	//
	// The body is deliberately invalid: the CSRF check runs as middleware,
	// before any handler, so a refusal here proves the gate and leaves the
	// password untouched for the rest of this test.
	res = withoutCSRF.do(http.MethodPost, "/api/auth/change-password", map[string]any{
		"current_password": "",
		"new_password":     "",
	})
	if res.status != http.StatusForbidden || res.errorCode() != "csrf_failed" {
		t.Fatalf("POST without CSRF: status = %d, code = %q, want 403 csrf_failed", res.status, res.errorCode())
	}

	// Reading is unaffected: the CSRF check applies only to unsafe methods.
	if res := c.do(http.MethodGet, "/api/me", nil); res.status != http.StatusOK {
		t.Fatalf("GET /api/me: status = %d, body = %s", res.status, res.raw)
	}

	if res := c.do(http.MethodPost, "/api/auth/logout", nil); res.status != http.StatusOK {
		t.Fatalf("logout: status = %d, body = %s", res.status, res.raw)
	}
	if res := c.do(http.MethodGet, "/api/me", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("/api/me after logout: status = %d, want 401", res.status)
	}
}

// A revoked session must stop working immediately, which is the whole reason
// sessions are server-side rather than a signed token.
func TestRevocationIsImmediate(t *testing.T) {
	env := newTestEnv(t)
	c := env.signupAndLogin("revoke@example.com")

	if res := c.do(http.MethodGet, "/api/me", nil); res.status != http.StatusOK {
		t.Fatalf("/api/me: status = %d", res.status)
	}

	// Revoke out of band, as an admin action or a sweeper would.
	if _, err := env.store.RevokeUserSessions(context.Background(), env.userID("revoke@example.com"), ""); err != nil {
		t.Fatalf("revoke sessions: %v", err)
	}

	if res := c.do(http.MethodGet, "/api/me", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("/api/me after revocation: status = %d, want 401", res.status)
	}
}

// Signup must not reveal whether an address is already registered.
func TestSignupDoesNotRevealExistingAccounts(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	const email = "taken@example.com"
	first := c.do(http.MethodPost, "/api/auth/signup", map[string]any{"email": email, "password": testPassword})
	sentAfterFirst := env.mailer.count()

	second := c.do(http.MethodPost, "/api/auth/signup", map[string]any{"email": email, "password": testPassword})

	if first.status != second.status {
		t.Fatalf("status differs: first = %d, second = %d", first.status, second.status)
	}
	if first.raw != second.raw {
		t.Fatalf("body differs:\nfirst:  %s\nsecond: %s", first.raw, second.raw)
	}
	// No second email either: that would confirm the address to whoever
	// triggered it.
	if env.mailer.count() != sentAfterFirst {
		t.Fatal("a second signup for the same address sent another email")
	}

	// And no duplicate row was created.
	var count int
	if err := env.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE email = $1`, email).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("users with that address = %d, want 1", count)
	}
}

// A wrong password and an unknown account must be indistinguishable.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	env := newTestEnv(t)
	c := env.signupAndLogin("real@example.com")

	wrongPassword := env.login(c, "real@example.com", "the-wrong-passphrase")
	unknownAccount := env.login(c, "nobody@example.com", "the-wrong-passphrase")

	if wrongPassword.status != http.StatusUnauthorized {
		t.Fatalf("wrong password: status = %d, want 401", wrongPassword.status)
	}
	if wrongPassword.status != unknownAccount.status {
		t.Fatalf("status differs: wrong password = %d, unknown account = %d",
			wrongPassword.status, unknownAccount.status)
	}
	if wrongPassword.raw != unknownAccount.raw {
		t.Fatalf("body differs:\nwrong password:  %s\nunknown account: %s",
			wrongPassword.raw, unknownAccount.raw)
	}
}

// Forgot-password must answer the same way for a registered and an unknown
// address.
func TestForgotPasswordDoesNotRevealAccounts(t *testing.T) {
	env := newTestEnv(t)
	c := env.signupAndLogin("known@example.com")

	known := c.do(http.MethodPost, "/api/auth/forgot-password", map[string]any{"email": "known@example.com"})
	unknown := c.do(http.MethodPost, "/api/auth/forgot-password", map[string]any{"email": "unknown@example.com"})

	if known.status != http.StatusAccepted {
		t.Fatalf("known address: status = %d, want 202", known.status)
	}
	if known.status != unknown.status || known.raw != unknown.raw {
		t.Fatalf("responses differ:\nknown:   %d %s\nunknown: %d %s",
			known.status, known.raw, unknown.status, unknown.raw)
	}
}

// A reset must set the new password and kill every session, including ones
// the attacker may be holding.
func TestPasswordResetRevokesEverySession(t *testing.T) {
	env := newTestEnv(t)
	const email = "reset@example.com"

	first := env.signupAndLogin(email)
	second := env.newClient()
	if res := env.login(second, email, testPassword); res.status != http.StatusOK {
		t.Fatalf("second login: status = %d, body = %s", res.status, res.raw)
	}

	if res := first.do(http.MethodPost, "/api/auth/forgot-password", map[string]any{"email": email}); res.status != http.StatusAccepted {
		t.Fatalf("forgot-password: status = %d, body = %s", res.status, res.raw)
	}
	token := tokenFrom(t, env.mailer.last(t))

	res := env.newClient().do(http.MethodPost, "/api/auth/reset-password", map[string]any{
		"token":    token,
		"password": testNewPassword,
	})
	if res.status != http.StatusOK {
		t.Fatalf("reset-password: status = %d, body = %s", res.status, res.raw)
	}

	// Both sessions are gone.
	for name, c := range map[string]*client{"first": first, "second": second} {
		if got := c.do(http.MethodGet, "/api/me", nil); got.status != http.StatusUnauthorized {
			t.Errorf("%s session after reset: status = %d, want 401", name, got.status)
		}
	}

	// The token cannot be spent twice.
	again := env.newClient().do(http.MethodPost, "/api/auth/reset-password", map[string]any{
		"token":    token,
		"password": "yet-another-passphrase",
	})
	if again.status != http.StatusBadRequest || again.errorCode() != "invalid_token" {
		t.Fatalf("reusing the reset token: status = %d, code = %q, want 400 invalid_token",
			again.status, again.errorCode())
	}

	// The old password is dead and the new one works.
	fresh := env.newClient()
	if res := env.login(fresh, email, testPassword); res.status != http.StatusUnauthorized {
		t.Fatalf("login with the old password: status = %d, want 401", res.status)
	}
	if res := env.login(fresh, email, testNewPassword); res.status != http.StatusOK {
		t.Fatalf("login with the new password: status = %d, body = %s", res.status, res.raw)
	}
}

func TestVerificationTokenIsSingleUse(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()
	token := env.signup(c, "once@example.com")

	if res := c.do(http.MethodPost, "/api/auth/verify-email", map[string]any{"token": token}); res.status != http.StatusOK {
		t.Fatalf("first use: status = %d, body = %s", res.status, res.raw)
	}
	res := c.do(http.MethodPost, "/api/auth/verify-email", map[string]any{"token": token})
	if res.status != http.StatusBadRequest || res.errorCode() != "invalid_token" {
		t.Fatalf("second use: status = %d, code = %q, want 400 invalid_token", res.status, res.errorCode())
	}
}

// Changing a password must require the current one and must not disturb the
// caller's own session.
func TestChangePassword(t *testing.T) {
	env := newTestEnv(t)
	const email = "change@example.com"
	c := env.signupAndLogin(email)

	other := env.newClient()
	if res := env.login(other, email, testPassword); res.status != http.StatusOK {
		t.Fatalf("second login: status = %d", res.status)
	}

	wrong := c.do(http.MethodPost, "/api/auth/change-password", map[string]any{
		"current_password": "not-the-current-password",
		"new_password":     testNewPassword,
	})
	if wrong.status != http.StatusForbidden {
		t.Fatalf("wrong current password: status = %d, body = %s, want 403", wrong.status, wrong.raw)
	}

	ok := c.do(http.MethodPost, "/api/auth/change-password", map[string]any{
		"current_password": testPassword,
		"new_password":     testNewPassword,
	})
	if ok.status != http.StatusOK {
		t.Fatalf("change password: status = %d, body = %s", ok.status, ok.raw)
	}

	// The caller keeps working; the other session does not.
	if res := c.do(http.MethodGet, "/api/me", nil); res.status != http.StatusOK {
		t.Fatalf("own session after change: status = %d, want 200", res.status)
	}
	if res := other.do(http.MethodGet, "/api/me", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("other session after change: status = %d, want 401", res.status)
	}
}

// A weak password must be refused on every path that sets one.
func TestWeakPasswordsAreRefused(t *testing.T) {
	env := newTestEnv(t)
	c := env.newClient()

	res := c.do(http.MethodPost, "/api/auth/signup", map[string]any{
		"email":    "weak@example.com",
		"password": "short",
	})
	if res.status != http.StatusBadRequest {
		t.Fatalf("short password at signup: status = %d, body = %s, want 400", res.status, res.raw)
	}
	errObj, _ := res.body["error"].(map[string]any)
	if param, _ := errObj["param"].(string); param != "password" {
		t.Fatalf("error param = %q, want password", param)
	}

	// Nothing was created.
	if _, err := env.store.UserByEmail(context.Background(), "weak@example.com"); err == nil {
		t.Fatal("a user was created despite the rejected password")
	}
}
