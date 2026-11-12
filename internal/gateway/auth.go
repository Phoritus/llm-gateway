package gateway

import (
	"errors"
	"net/http"
	"strings"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

func (d Deps) authenticate(w http.ResponseWriter, r *http.Request) (store.ActiveGatewayKey, bool) {
	token, ok := bearerToken(r)
	if !ok {
		unauthorized(w, "You didn't provide an API key. Send your gateway key as `Authorization: Bearer gwk_...`.")
		return store.ActiveGatewayKey{}, false
	}
	key, err := d.Keys.Authenticate(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidGatewayKey) {
		unauthorized(w, "The gateway key is invalid or has been revoked.")
		return store.ActiveGatewayKey{}, false
	}
	if err != nil {
		d.serverError(w, r, err, "authenticate gateway key")
		return store.ActiveGatewayKey{}, false
	}
	if !key.OwnerVerified {
		writeError(w, http.StatusForbidden, typePermission, "email_not_verified",
			"Confirm the workspace owner's email address before using this gateway.")
		return store.ActiveGatewayKey{}, false
	}
	return key, true
}

// bearerToken reads "Authorization: Bearer <token>". The scheme is matched
// without regard to case, as RFC 9110 requires.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="llm-gateway"`)
	writeError(w, http.StatusUnauthorized, typeInvalidRequest, "invalid_api_key", message)
}
