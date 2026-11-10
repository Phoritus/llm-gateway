package auth

import (
	"context"
	"errors"
	"net/http"

	"llm-gateway/internal/store"
)

// contextKey is unexported so no other package can write these values. The
// only way an identity enters a request context is through RequireSession.
type contextKey int

const identityKey contextKey = iota

// ErrorFunc writes an error response. The auth package takes these as
// callbacks so it never has to know the control plane's error shape, which
// keeps this package free of an import cycle with internal/api.
type ErrorFunc func(http.ResponseWriter, *http.Request)

// ServerErrorFunc writes a 500 and is handed the cause. It is a separate shape
// from ErrorFunc because the layer holding the error is the only layer that can
// log it: given an ErrorFunc, RequireSession could report a Postgres outage
// during session resolution only as an unexplained 500, which is the one case
// where the request id in the response body buys the reader nothing.
type ServerErrorFunc func(http.ResponseWriter, *http.Request, error)

// RequireSession authenticates the request and attaches the caller's
// identity. Downstream handlers read the workspace id from that identity and
// never from the request body or query, which is what keeps one tenant's
// handler from touching another's rows.
func (s *Service) RequireSession(unauthorized ErrorFunc, serverError ServerErrorFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, err := s.ResolveSession(r.Context(), s.SessionTokenFromRequest(r))
			if err != nil {
				if errors.Is(err, ErrNoSession) {
					// Clear the cookie on the way out: it is known-dead, and
					// leaving it set makes every later request pay for a
					// lookup that cannot succeed.
					s.ClearSessionCookies(w)
					unauthorized(w, r)
					return
				}
				serverError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
		})
	}
}

// RequireVerifiedEmail refuses a caller whose email address has not been
// confirmed. It must be mounted inside RequireSession, which is what puts the
// identity it reads into the context.
//
// This is the gate that makes verification mean something: without it the flag
// is reported by /me and enforced nowhere, which is indistinguishable from
// having decided not to require it. Account hygiene stays outside this group on
// purpose -- /me, logout, logout-all and change-password -- because a user who
// cannot yet use the service must still be able to see why, sign out and
// secure their account. Everything that spends the service sits inside it.
//
// A request with no identity at all is refused rather than passed through. That
// state means this middleware was mounted without RequireSession above it,
// which is a wiring mistake, and a wiring mistake in an authorisation check
// must not fail open.
func RequireVerifiedEmail(onUnverified ErrorFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := IdentityFrom(r.Context())
			if !ok || !identity.EmailVerified() {
				onUnverified(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WithIdentity attaches an identity to a context. Exported for tests that
// exercise a handler without going through the middleware.
func WithIdentity(ctx context.Context, identity *store.Identity) context.Context {
	return context.WithValue(ctx, identityKey, identity)
}

// IdentityFrom returns the authenticated caller, if the request passed
// through RequireSession.
func IdentityFrom(ctx context.Context) (*store.Identity, bool) {
	identity, ok := ctx.Value(identityKey).(*store.Identity)
	return identity, ok
}

// UserID returns the authenticated user's id, or "" when unauthenticated.
func UserID(ctx context.Context) string {
	if identity, ok := IdentityFrom(ctx); ok {
		return identity.Session.UserID
	}
	return ""
}

// WorkspaceID returns the caller's active workspace. Every tenant-scoped
// query in a handler must take its workspace id from here.
func WorkspaceID(ctx context.Context) string {
	if identity, ok := IdentityFrom(ctx); ok {
		return identity.Session.WorkspaceID
	}
	return ""
}
