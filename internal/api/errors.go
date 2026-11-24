package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"llm-gateway/internal/telemetry"
)

// Error types, matching OpenAI's vocabulary. The control plane uses the same
// envelope as /v1 so that one client error handler covers both planes, which
// is the invariant the spec states for errors.
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typePermission     = "permission_error"
	typeNotFound       = "not_found_error"
	typeRateLimit      = "rate_limit_error"
	typeServer         = "server_error"
)

// errorBody is the OpenAI error envelope.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	respondJSON(w, status, body, true)
}

// writeJSONUnescaped leaves <, > and & as they are, for bodies full of
// stored text, where each would otherwise grow to six bytes.
func writeJSONUnescaped(w http.ResponseWriter, status int, body any) {
	respondJSON(w, status, body, false)
}

func respondJSON(w http.ResponseWriter, status int, body any, escapeHTML bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Error and account responses must never be cached by a proxy or the
	// browser's back button.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(body); err != nil {
		// The status line is already sent, so there is nothing to do but
		// record it.
		slog.Error("encode response", slog.String("error", err.Error()))
	}
}

func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Message: message,
		Type:    errType,
		Code:    code,
	}})
}

func writeFieldError(w http.ResponseWriter, status int, errType, code, param, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Message: message,
		Type:    errType,
		Param:   &param,
		Code:    code,
	}})
}

func badRequest(w http.ResponseWriter, code, message string) {
	writeError(w, http.StatusBadRequest, typeInvalidRequest, code, message)
}

func invalidField(w http.ResponseWriter, param, message string) {
	writeFieldError(w, http.StatusBadRequest, typeInvalidRequest, "invalid_value", param, message)
}

// unauthorized is the single answer to every failed authentication: an absent
// cookie, an unknown token, a revoked session and an expired one all look
// identical from outside.
func unauthorized(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusUnauthorized, typeAuthentication, "not_authenticated",
		"You must sign in to use this endpoint.")
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, typeNotFound, "not_found",
		"The requested resource does not exist.")
}

func csrfFailed(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusForbidden, typePermission, "csrf_failed",
		"Missing or invalid "+csrfHeaderName+" header.")
}

// emailNotVerified is the answer to an authenticated request from an account
// that has not confirmed its address. It is 403 rather than 401: the session is
// valid and signing in again will not help, so a client that retries
// authentication would loop.
func emailNotVerified(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusForbidden, typePermission, "email_not_verified",
		"Confirm your email address to use this endpoint. "+
			"Check your inbox, or request a new link at /api/auth/resend-verification.")
}

func originRejected(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusForbidden, typePermission, "origin_not_allowed",
		"This origin may not make state-changing requests.")
}

func tooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	// Retry-After is in seconds and must be at least 1, or a client reading it
	// as a number gets told to retry immediately.
	seconds := int(retryAfter.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, typeRateLimit, "rate_limit_exceeded",
		"Too many requests. Try again in "+strconv.Itoa(seconds)+"s.")
}

// serverErrorf logs the real cause and returns an opaque message. Internal
// detail — a driver error, a constraint name, a query — tells an attacker
// about the schema and is of no use to a legitimate client, who has the
// request id to quote instead.
func (d Deps) serverErrorf(w http.ResponseWriter, r *http.Request, err error, what string) {
	d.logger().ErrorContext(r.Context(), what,
		slog.String("request_id", telemetry.RequestIDFrom(r.Context())),
		slog.String("error", err.Error()),
	)
	writeError(w, http.StatusInternalServerError, typeServer, "internal_error",
		"Something went wrong on our side. Quote request id "+
			telemetry.RequestIDFrom(r.Context())+" if it keeps happening.")
}
