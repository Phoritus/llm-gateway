// Package api holds the control plane handlers served under /api.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

// csrfHeaderName is referenced in error messages.
const csrfHeaderName = auth.CSRFHeader

// maxBodyBytes caps a control plane request body. These are small JSON
// documents; anything larger is either a mistake or an attempt to make the
// server allocate.
const maxBodyBytes = 64 << 10

// Deps is everything the control plane needs. It is passed by value; all
// fields are pointers or interfaces that are safe to share.
type Deps struct {
	Config      *config.Config
	Store       *store.Store
	Credentials *store.Credentials
	Auth        *auth.Service
	Limiter     *ratelimit.Limiter
	Mailer      auth.Mailer
	Logger      *slog.Logger
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Routes builds the /api handler.
//
// The middleware order matters. CORS answers preflights before anything else;
// the same-origin check then covers every state-changing request including
// the unauthenticated ones, which the CSRF token cannot protect because no
// session exists yet; inside the authenticated group, RequireSession runs
// before RequireCSRF so that a request without a session is answered 401
// rather than 403.
func Routes(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(routeTelemetry)
	r.Use(corsMiddleware(d.Config.AllowedOrigins))
	r.Use(auth.RequireSameOrigin(d.Config.AllowedOrigins, originRejected))

	r.Route("/auth", func(r chi.Router) {
		r.Post("/signup", d.handleSignup)
		r.Post("/login", d.handleLogin)
		r.Post("/logout", d.handleLogout)
		// The verification link in an email is a GET, so both shapes are
		// served: the link itself, and a POST for a dashboard that collects
		// the token from the URL and submits it.
		r.Get("/verify-email", d.handleVerifyEmail)
		r.Post("/verify-email", d.handleVerifyEmail)
		r.Post("/resend-verification", d.handleResendVerification)
		r.Post("/forgot-password", d.handleForgotPassword)
		r.Post("/reset-password", d.handleResetPassword)
	})

	r.Group(func(r chi.Router) {
		r.Use(d.Auth.RequireSession(unauthorized, func(w http.ResponseWriter, r *http.Request, err error) {
			d.serverErrorf(w, r, err, "resolve session")
		}))
		r.Use(d.Auth.RequireCSRF(csrfFailed))

		// Metering the group, not each handler: there is then no way to add an
		// authenticated route that forgets to be metered.
		r.Use(d.limitPerUser)

		// Reachable without a confirmed address. A user who cannot yet use the
		// service must still be able to see that, sign out everywhere, and
		// change their password -- the three things someone does when they
		// suspect the account is not only theirs.
		r.Get("/me", d.handleMe)
		r.Post("/auth/change-password", d.handleChangePassword)
		r.Post("/auth/logout-all", d.handleLogoutAll)

		// Everything that actually spends the service requires a confirmed
		// address. The gate is on the group for the same reason the rate limit
		// is: a route added here inherits it, and a route added here having
		// forgotten it is not a thing that can happen.
		//
		// Gateway keys, route aliases and usage mount here as they land, and
		// /v1 enforces the same rule through the gateway key's workspace
		// rather than through a session.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireVerifiedEmail(emailNotVerified))

			r.Get("/credentials", d.handleListCredentials)
			r.Post("/credentials", d.handleCreateCredential)
			r.Delete("/credentials/{id}", d.handleDeleteCredential)
		})
	})

	r.NotFound(notFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, typeInvalidRequest, "method_not_allowed",
			"That method is not allowed on this endpoint.")
	})
	return r
}

// routeTelemetry reports the matched chi pattern to the telemetry layer. It
// reads the pattern after the handlers have run, because chi fills it in
// while routing, and it is first in the chain so the preflights and rejections
// the later middleware answers are labelled too. chi sees the path with /api
// already stripped, so the prefix is put back.
func routeTelemetry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		route := telemetry.UnmatchedRoute
		if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
			route = "/api" + pattern
		}
		telemetry.SetRoute(r.Context(), route)
	})
}

// corsMiddleware allows the dashboard origin with credentials. It is written
// out rather than pulled in as a dependency because the policy is three
// lines: an exact origin match, credentials on, and a small header allowlist.
// A wildcard origin is impossible here by construction, which matters because
// a wildcard plus credentials is exactly the mistake that would expose every
// session.
//
// Whether an Origin matches is config.Origins.Allows, never a rule of this
// package's own. It was once both: this function kept its own
// lowercase-and-trim-slash copy, which disagreed with Allows on a
// whitespace-padded origin -- unreachable over HTTP, since net/http strips
// optional whitespace from a header value, but a drift the Origins type exists
// to make impossible rather than merely unlikely.
func corsMiddleware(allowed config.Origins) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if allowed.Allows(origin) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Allow-Headers", "Content-Type, "+auth.CSRFHeader)
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Max-Age", "600")
				// The response varies by origin, so a shared cache must not
				// serve one origin's headers to another.
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// decodeJSON reads a request body into dst with a size cap, rejecting a body
// that carries a field dst has no place for. Rejecting unknown fields turns a
// typo in a client into a visible error instead of a silently ignored setting.
//
// The body is parsed twice, once into a map of its top-level keys and once into
// dst, which is how the unknown-field check comes to be this package's own.
// json.Decoder.DisallowUnknownFields reports the offence only in prose, with no
// typed error, so classifying it meant matching on a standard-library sentence
// and handing that sentence to the client -- the single place an internal error
// message reached a response. Two passes over a document the cap holds to 64 KiB
// are affordable for human-rate control-plane endpoints. They would not be for
// /v1, which does not come through here.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, typeInvalidRequest, "unsupported_media_type",
				"Content-Type must be application/json.")
			return false
		}
	}

	// MaxBytesReader rather than io.LimitReader: it reports the overflow as a
	// distinct error instead of handing the parser a truncated document, so an
	// oversized body is answered as oversized.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, typeInvalidRequest, "body_too_large",
				"The request body is too large.")
			return false
		}
		badRequest(w, "invalid_body", "The request body could not be read.")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		badRequest(w, "missing_body", "A JSON body is required.")
		return false
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		badRequest(w, "invalid_json", "The request body must be a single JSON object.")
		return false
	}
	if unknown := unknownFields(dst, fields); len(unknown) > 0 {
		badRequest(w, "unknown_field",
			"This endpoint does not accept the field "+strconv.Quote(unknown[0])+".")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		badRequest(w, "invalid_json", "The request body is not valid JSON.")
		return false
	}
	return true
}

// unknownFields reports, in a stable order, the body keys that dst has no place
// for.
//
// Names are compared case-insensitively because that is how encoding/json
// matches them: it accepts {"Email": ...} for a field tagged "email", and this
// check replaces its reporting, not its contract. An unexported field and one
// tagged "-" are both invisible to encoding/json, so neither makes a key known
// -- registering them would let a body carry that key and have it dropped in
// silence, which is the outcome rejecting unknown fields exists to prevent.
//
// A dst that is not a pointer to a struct has no known fields, so every key is
// unknown and the request is refused. That state is a wiring mistake, and a
// wiring mistake inside a validator must not fail open.
func unknownFields(dst any, fields map[string]json.RawMessage) []string {
	known := make(map[string]struct{})
	if t := reflect.TypeOf(dst); t != nil && t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct {
		t = t.Elem()
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			known[strings.ToLower(name)] = struct{}{}
		}
	}

	var unknown []string
	for key := range fields {
		if _, ok := known[strings.ToLower(key)]; !ok {
			unknown = append(unknown, key)
		}
	}
	// Sorted so the field named in the response does not depend on Go's map
	// iteration order, which would make the same request answer differently
	// from one call to the next.
	slices.Sort(unknown)
	return unknown
}

// limitPerUser applies the authenticated ceiling, keyed on the caller's user
// id. It must run after RequireSession, which is what puts the identity in the
// context, and after RequireCSRF, so a request refused for a missing token
// does not spend the caller's budget.
func (d Deps) limitPerUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.allow(w, r, "api:user", auth.UserID(r.Context()), apiPerUser) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow checks one rate limit bucket and writes the 429 itself when the
// request must be refused.
//
// A Redis failure refuses the request rather than letting it through. The
// alternative — failing open — would mean an attacker who can make Redis
// unavailable also gets unmetered password guessing. Readiness already
// depends on Redis, so an outage takes the instance out of rotation anyway.
func (d Deps) allow(w http.ResponseWriter, r *http.Request, scope, id string, rule ratelimit.Rule) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	decision, err := d.Limiter.AllowN(ctx, scope, id, rule, 1)
	if err != nil {
		d.logger().ErrorContext(r.Context(), "rate limit check failed",
			slog.String("scope", scope), slog.String("error", err.Error()))
		writeError(w, http.StatusServiceUnavailable, typeServer, "rate_limiter_unavailable",
			"The service is temporarily unable to process this request.")
		return false
	}
	if !decision.Allowed {
		tooManyRequests(w, decision.RetryAfter)
		return false
	}
	return true
}
