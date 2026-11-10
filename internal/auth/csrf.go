package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"

	"llm-gateway/internal/config"
)

// CSRFHeader is the header the dashboard must echo the CSRF token in.
const CSRFHeader = "X-CSRF-Token"

// csrfContext separates this HMAC's inputs from any other use of the session
// secret, so a value from one can never be replayed as the other.
const csrfContext = "csrf-v1:"

// CSRFToken derives the CSRF token for a session token. It is an HMAC of the
// session token under the service's secret, which gives a property plain
// double-submit lacks: the token is bound to one session and the server can
// recompute it, so verification never trusts a value the client sent twice.
// An attacker who can write cookies but cannot read the session token still
// cannot produce a matching header.
func (s *Service) CSRFToken(sessionToken string) string {
	mac := hmac.New(sha256.New, s.cfg.SessionSecret)
	mac.Write([]byte(csrfContext))
	mac.Write([]byte(sessionToken))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyCSRF checks the submitted header against the token recomputed from
// the session cookie.
func (s *Service) VerifyCSRF(r *http.Request) bool {
	sessionToken := s.SessionTokenFromRequest(r)
	if sessionToken == "" {
		return false
	}
	presented := r.Header.Get(CSRFHeader)
	if presented == "" {
		return false
	}
	return hmac.Equal([]byte(presented), []byte(s.CSRFToken(sessionToken)))
}

// safeMethod reports whether a method is defined not to change state, and so
// needs no CSRF token.
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// RequireCSRF rejects state-changing requests that do not carry a valid CSRF
// token. It is mounted on the authenticated part of the control plane, where
// a session cookie exists to bind the token to.
func (s *Service) RequireCSRF(onFail func(http.ResponseWriter, *http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !safeMethod(r.Method) && !s.VerifyCSRF(r) {
				onFail(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSameOrigin rejects state-changing requests whose Origin, or Referer
// where Origin is absent, is not an allowed dashboard origin.
//
// This covers the endpoints that have no session yet — login, signup, password
// reset — which RequireCSRF cannot protect because there is no session token
// to bind a token to. Without it, a third-party page could log a victim into
// an account the attacker controls.
//
// A request with neither header is allowed through: those are non-browser
// clients such as curl, which are not subject to cross-site request forgery
// in the first place, and blocking them would break scripted use.
func RequireSameOrigin(allowed config.Origins, onFail ErrorFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if origin == "" {
				if ref := r.Header.Get("Referer"); ref != "" {
					if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
						origin = u.Scheme + "://" + u.Host
					} else {
						// A Referer we cannot parse is not evidence of
						// anything, so treat it as a mismatch.
						onFail(w, r)
						return
					}
				}
			}
			if origin == "" || allowed.Allows(origin) {
				next.ServeHTTP(w, r)
				return
			}
			onFail(w, r)
		})
	}
}
