package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm-gateway/internal/store"
)

func identity(verifiedAt *time.Time) *store.Identity {
	return &store.Identity{
		Session:         store.Session{UserID: "user-1", WorkspaceID: "ws-1"},
		Email:           "user@example.com",
		EmailVerifiedAt: verifiedAt,
	}
}

// RequireVerifiedEmail is the whole of what makes verification mandatory, so
// each of its three outcomes is pinned here rather than inferred from a route.
func TestRequireVerifiedEmail(t *testing.T) {
	verified := time.Now().Add(-time.Hour)

	cases := []struct {
		name       string
		identity   *store.Identity
		attach     bool
		wantCalled bool
	}{
		{name: "verified caller passes", identity: identity(&verified), attach: true, wantCalled: true},
		{name: "unverified caller is refused", identity: identity(nil), attach: true, wantCalled: false},
		// No identity means the middleware was mounted without RequireSession
		// above it. That is a wiring mistake, and an authorisation check must
		// not fail open on one.
		{name: "no identity is refused", attach: false, wantCalled: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})

			var refused bool
			handler := RequireVerifiedEmail(func(w http.ResponseWriter, r *http.Request) {
				refused = true
				w.WriteHeader(http.StatusForbidden)
			})(next)

			req := httptest.NewRequest(http.MethodGet, "/gated", nil)
			if tc.attach {
				req = req.WithContext(WithIdentity(req.Context(), tc.identity))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if called != tc.wantCalled {
				t.Errorf("next handler called = %v, want %v", called, tc.wantCalled)
			}
			if refused == tc.wantCalled {
				t.Errorf("refusal called = %v, want %v", refused, !tc.wantCalled)
			}
			wantStatus := http.StatusForbidden
			if tc.wantCalled {
				wantStatus = http.StatusOK
			}
			if rec.Code != wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, wantStatus)
			}
		})
	}
}
