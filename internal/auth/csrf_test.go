package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llm-gateway/internal/config"
)

func testService(secure bool, domain string) *Service {
	return NewService(nil, Config{
		Secure:        secure,
		CookieDomain:  domain,
		SessionSecret: []byte("a test session secret of ample length"),
	})
}

func TestCSRFTokenIsBoundToTheSession(t *testing.T) {
	svc := testService(false, "")

	first := svc.CSRFToken("session-token-one")
	second := svc.CSRFToken("session-token-two")

	if first == "" {
		t.Fatal("CSRFToken returned an empty string")
	}
	if first == second {
		t.Fatal("two different session tokens produced the same CSRF token")
	}
	if first != svc.CSRFToken("session-token-one") {
		t.Fatal("CSRFToken is not deterministic for one session token")
	}

	// A different secret must produce a different token, or the HMAC is not
	// keyed by the configured secret at all.
	other := NewService(nil, Config{SessionSecret: []byte("a different secret, also long enough")})
	if other.CSRFToken("session-token-one") == first {
		t.Fatal("CSRF token does not depend on the session secret")
	}
}

func TestVerifyCSRF(t *testing.T) {
	svc := testService(false, "")
	const sessionToken = "the-session-token"
	validCSRF := svc.CSRFToken(sessionToken)

	withCookie := func(r *http.Request) *http.Request {
		r.AddCookie(&http.Cookie{Name: svc.SessionCookieName(), Value: sessionToken})
		return r
	}

	t.Run("accepts the matching header", func(t *testing.T) {
		r := withCookie(httptest.NewRequest(http.MethodPost, "/api/me", nil))
		r.Header.Set(CSRFHeader, validCSRF)
		if !svc.VerifyCSRF(r) {
			t.Fatal("VerifyCSRF rejected a valid token")
		}
	})

	t.Run("rejects a missing header", func(t *testing.T) {
		r := withCookie(httptest.NewRequest(http.MethodPost, "/api/me", nil))
		if svc.VerifyCSRF(r) {
			t.Fatal("VerifyCSRF accepted a request with no CSRF header")
		}
	})

	t.Run("rejects a wrong header", func(t *testing.T) {
		r := withCookie(httptest.NewRequest(http.MethodPost, "/api/me", nil))
		r.Header.Set(CSRFHeader, "not-the-token")
		if svc.VerifyCSRF(r) {
			t.Fatal("VerifyCSRF accepted a wrong CSRF token")
		}
	})

	// This is the property plain double-submit lacks: an attacker who can
	// write cookies but cannot read the session token must not be able to
	// produce a matching pair. Here the CSRF value belongs to another session.
	t.Run("rejects a token minted for another session", func(t *testing.T) {
		r := withCookie(httptest.NewRequest(http.MethodPost, "/api/me", nil))
		r.Header.Set(CSRFHeader, svc.CSRFToken("some-other-session"))
		if svc.VerifyCSRF(r) {
			t.Fatal("VerifyCSRF accepted a token bound to a different session")
		}
	})

	t.Run("rejects a request with no session", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/me", nil)
		r.Header.Set(CSRFHeader, validCSRF)
		if svc.VerifyCSRF(r) {
			t.Fatal("VerifyCSRF accepted a request with no session cookie")
		}
	})
}

func TestRequireCSRFLetsSafeMethodsThrough(t *testing.T) {
	svc := testService(false, "")
	var reached bool
	handler := svc.RequireCSRF(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		reached = false
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, "/api/me", nil))
		if !reached {
			t.Errorf("%s was blocked by the CSRF check", method)
		}
	}

	// And a state-changing method without a token must not reach the handler.
	reached = false
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/me", nil))
	if reached {
		t.Fatal("POST without a CSRF token reached the handler")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestRequireSameOrigin(t *testing.T) {
	// The list as config.Load produces it: lowercased and without a trailing
	// slash. Normalizing a hand-written entry is the loader's job and has its
	// own test there; this middleware is handed the result, which is what stops
	// the two readers of the list from disagreeing about what matches.
	allowed := config.Origins{"https://dash.example.com", "http://localhost:3000"}

	cases := []struct {
		name       string
		method     string
		header     string
		value      string
		wantPassed bool
	}{
		{"get from anywhere", http.MethodGet, "Origin", "https://evil.example", true},
		{"post from an allowed origin", http.MethodPost, "Origin", "https://dash.example.com", true},
		// Browsers are not consistent about case, and the entry is lowercase.
		{"post from an allowed origin spelled in capitals", http.MethodPost, "Origin", "HTTPS://DASH.EXAMPLE.COM", true},
		{"post from an allowed origin with a trailing slash", http.MethodPost, "Origin", "http://localhost:3000/", true},
		{"post from another origin", http.MethodPost, "Origin", "https://evil.example", false},
		{"post with an allowed referer", http.MethodPost, "Referer", "https://dash.example.com/settings", true},
		{"post with a foreign referer", http.MethodPost, "Referer", "https://evil.example/page", false},
		{"post with an unparseable referer", http.MethodPost, "Referer", "::::", false},
		// A non-browser client such as curl sends neither header and is not
		// subject to forgery, so it is allowed through.
		{"post with no origin or referer", http.MethodPost, "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var passed bool
			handler := RequireSameOrigin(allowed, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true }))

			r := httptest.NewRequest(tc.method, "/api/auth/login", nil)
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			handler.ServeHTTP(httptest.NewRecorder(), r)

			if passed != tc.wantPassed {
				t.Fatalf("passed = %v, want %v", passed, tc.wantPassed)
			}
		})
	}
}

// The cookie prefixes carry security meaning that the browser enforces, so the
// name must match the attributes exactly.
func TestCookieNamesMatchTheirAttributes(t *testing.T) {
	cases := []struct {
		name       string
		secure     bool
		domain     string
		wantName   string
		wantDomain string
	}{
		{"development", false, "", "gw_session", ""},
		{"production, host-only", true, "", "__Host-gw_session", ""},
		{"production with a domain", true, "example.com", "__Secure-gw_session", "example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := testService(tc.secure, tc.domain)
			if got := svc.SessionCookieName(); got != tc.wantName {
				t.Fatalf("SessionCookieName = %q, want %q", got, tc.wantName)
			}

			w := httptest.NewRecorder()
			svc.SetSessionCookies(w, "a-session-token")

			cookies := w.Result().Cookies()
			if len(cookies) != 2 {
				t.Fatalf("got %d cookies, want 2", len(cookies))
			}

			session, csrf := cookies[0], cookies[1]
			if session.Name != tc.wantName {
				t.Errorf("session cookie name = %q, want %q", session.Name, tc.wantName)
			}
			if !session.HttpOnly {
				t.Error("session cookie is not HttpOnly")
			}
			if session.Secure != tc.secure {
				t.Errorf("session cookie Secure = %v, want %v", session.Secure, tc.secure)
			}
			if session.Domain != tc.wantDomain {
				t.Errorf("session cookie Domain = %q, want %q", session.Domain, tc.wantDomain)
			}
			if session.Path != "/" {
				// __Host- requires Path=/, so this is not cosmetic.
				t.Errorf("session cookie Path = %q, want /", session.Path)
			}
			if session.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie SameSite = %v, want Lax", session.SameSite)
			}

			if csrf.HttpOnly {
				t.Error("CSRF cookie is HttpOnly, so the dashboard could not read it")
			}
			if csrf.Value != svc.CSRFToken("a-session-token") {
				t.Error("CSRF cookie does not carry the token for this session")
			}
		})
	}
}

func TestClearSessionCookiesExpiresBoth(t *testing.T) {
	svc := testService(false, "")
	w := httptest.NewRecorder()
	svc.ClearSessionCookies(w)

	cookies := w.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("got %d cookies, want 2", len(cookies))
	}
	for _, c := range cookies {
		if c.Value != "" {
			t.Errorf("cookie %q still carries a value", c.Name)
		}
		if c.MaxAge >= 0 {
			t.Errorf("cookie %q MaxAge = %d, want a negative value to expire it", c.Name, c.MaxAge)
		}
	}
}
