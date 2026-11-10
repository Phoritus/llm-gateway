package auth

import (
	"net/http"
)

// Cookie names. The prefixes are enforced by the browser, not by us:
//
//   - __Host- requires Secure, Path=/ and no Domain, and forbids a subdomain
//     from writing the cookie at all. That last part is what makes the CSRF
//     double-submit safe, so it is the default in production.
//   - __Secure- only requires Secure, and is the fallback when the deployment
//     needs a Domain to share cookies across subdomains.
//   - The bare names are for development over plain http, where neither
//     prefix is accepted.
const (
	sessionCookieBase = "gw_session"
	csrfCookieBase    = "gw_csrf"

	hostPrefix   = "__Host-"
	securePrefix = "__Secure-"
)

// SessionCookieName returns the name in use for the current configuration.
func (s *Service) SessionCookieName() string { return s.cookieName(sessionCookieBase) }

// CSRFCookieName returns the name of the readable CSRF cookie.
func (s *Service) CSRFCookieName() string { return s.cookieName(csrfCookieBase) }

func (s *Service) cookieName(base string) string {
	switch {
	case s.cfg.Secure && s.cfg.CookieDomain == "":
		return hostPrefix + base
	case s.cfg.Secure:
		return securePrefix + base
	default:
		return base
	}
}

// SetSessionCookies writes both cookies for a newly created session: the
// HttpOnly session token, and the readable CSRF token derived from it.
//
// Path is "/" rather than "/api" because the __Host- prefix requires it. The
// cookie is still only ever read by handlers under /api.
func (s *Service) SetSessionCookies(w http.ResponseWriter, token string) {
	http.SetCookie(w, s.cookie(s.SessionCookieName(), token, true, int(s.cfg.SessionTTL.Seconds())))
	// Readable by the dashboard's JavaScript, which must echo it in the
	// X-CSRF-Token header. It is not a secret on its own: it is worthless
	// without the HttpOnly session cookie it is bound to.
	http.SetCookie(w, s.cookie(s.CSRFCookieName(), s.CSRFToken(token), false, int(s.cfg.SessionTTL.Seconds())))
}

// ClearSessionCookies expires both cookies. Logout calls this in addition to
// revoking the row, so a client that ignores the response still cannot reuse
// the cookie it holds.
func (s *Service) ClearSessionCookies(w http.ResponseWriter) {
	http.SetCookie(w, s.cookie(s.SessionCookieName(), "", true, -1))
	http.SetCookie(w, s.cookie(s.CSRFCookieName(), "", false, -1))
}

// SessionTokenFromRequest reads the presented session token, if any.
func (s *Service) SessionTokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(s.SessionCookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Service) cookie(name, value string, httpOnly bool, maxAge int) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   s.cfg.Secure,
		// Lax, not Strict: Strict would drop the cookie when the user follows
		// the verification or reset link from their mail client, which is a
		// top-level navigation to our own site. Lax sends it there and still
		// withholds it from cross-site POSTs; the CSRF token covers the rest.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
	// Setting a Domain is what switches the name from __Host- to __Secure-,
	// so the two always agree: a __Host- cookie must carry no Domain.
	if s.cfg.CookieDomain != "" {
		c.Domain = s.cfg.CookieDomain
	}
	return c
}
