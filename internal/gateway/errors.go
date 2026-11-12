package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/telemetry"
)

// OpenAI's error types. The control plane writes the same envelope; it is a
// fixed external shape, so the two copies have nothing to drift on.
const (
	typeInvalidRequest = "invalid_request_error"
	typeNotFound       = "not_found_error"
	typePermission     = "permission_error"
	typeRateLimit      = "rate_limit_error"
	typeServer         = "server_error"
)

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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("encode response", slog.String("error", err.Error()))
	}
}

func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: message, Type: errType, Code: code}})
}

func invalidField(w http.ResponseWriter, param, message string) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{
		Message: message, Type: typeInvalidRequest, Param: &param, Code: "invalid_value",
	}})
}

// upstreamError is how an upstream failure is told to a client: as a status
// line before the first byte, or as a final SSE event after it. Upstream
// trouble is never a 500 -- the gateway did its job, the upstream did not.
func upstreamError(err error) (int, errorDetail) {
	switch {
	case errors.Is(err, provider.ErrRateLimited), errors.Is(err, provider.ErrQuota):
		return http.StatusTooManyRequests, errorDetail{Type: typeRateLimit, Code: "upstream_rate_limited",
			Message: "The upstream provider is rate limiting requests. Try again later."}
	case errors.Is(err, provider.ErrContextTooLong):
		return http.StatusRequestEntityTooLarge, errorDetail{Type: typeInvalidRequest, Code: "context_length_exceeded",
			Message: "The request is longer than the model's context window."}
	case errors.Is(err, provider.ErrInvalidRequest):
		return http.StatusBadRequest, errorDetail{Type: typeInvalidRequest, Code: "upstream_invalid_request",
			Message: "The upstream provider rejected the request as invalid. Check the parameters it was sent."}
	case errors.Is(err, provider.ErrModelUnavailable):
		return http.StatusBadGateway, errorDetail{Type: typeServer, Code: "upstream_model_unavailable",
			Message: "The upstream provider does not offer this route's model to its credential."}
	case errors.Is(err, provider.ErrTimeout), errors.Is(err, errDeadline):
		return http.StatusBadGateway, errorDetail{Type: typeServer, Code: "upstream_timeout",
			Message: "The upstream provider did not answer in time."}
	default:
		return http.StatusBadGateway, errorDetail{Type: typeServer, Code: "upstream_error",
			Message: "The upstream provider failed to answer."}
	}
}

// writeUpstreamError answers a failure that happened before the first byte.
func (d Deps) writeUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	d.logUpstream(r, err)
	status, detail := upstreamError(err)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(provider.RetryAfter(err))))
	}
	writeJSON(w, status, errorBody{Error: detail})
}

func (d Deps) serverError(w http.ResponseWriter, r *http.Request, err error, op string) {
	if clientGone(r.Context()) {
		return
	}
	d.logger().ErrorContext(r.Context(), op+" failed",
		slog.String("request_id", telemetry.RequestIDFrom(r.Context())),
		slog.String("error", err.Error()))
	writeError(w, http.StatusInternalServerError, typeServer, "internal_error",
		"The gateway failed to process the request.")
}

func (d Deps) logUpstream(r *http.Request, err error) {
	d.logger().WarnContext(r.Context(), "upstream failed",
		slog.String("request_id", telemetry.RequestIDFrom(r.Context())),
		slog.String("error", err.Error()),
	)
}

// retryAfterSeconds is at least one, so a client never reads "retry now".
// Without a hint from the upstream it says a second.
func retryAfterSeconds(d time.Duration) int {
	return max(1, int(d.Round(time.Second)/time.Second))
}
