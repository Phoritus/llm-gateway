package gateway

import (
	"errors"
	"net/http"
	"strings"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

// authenticate resolves the bearer key, writing the refusal itself.
//
// A missing or invalid key is counted against the client's address, and an
// address past that limit is refused before its key is looked at, so a flood
// of unknown keys stops reaching Postgres. An unverified owner and a server
// error are not counted: neither is a guess at a key.
func (d Deps) authenticate(w http.ResponseWriter, r *http.Request) (store.ActiveGatewayKey, bool) {
	ip := d.ClientKey(r)
	if wait := d.blocks.wait(ip); wait > 0 {
		tooManyRequests(w, wait)
		return store.ActiveGatewayKey{}, false
	}

	token, ok := bearerToken(r)
	if !ok {
		if d.countAuthFailure(w, r, ip) {
			unauthorized(w, "You didn't provide an API key. Send your gateway key as `Authorization: Bearer gwk_...`.")
		}
		return store.ActiveGatewayKey{}, false
	}
	key, err := d.Keys.Authenticate(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidGatewayKey) {
		if d.countAuthFailure(w, r, ip) {
			unauthorized(w, "The gateway key is invalid or has been revoked.")
		}
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
