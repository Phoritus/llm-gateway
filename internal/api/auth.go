package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/store"
)

// Rate limits for the auth endpoints. Each is deliberately low: these are
// human actions, and the cost of a wrong guess should grow quickly. Buckets
// are keyed per address and per account so that one noisy client cannot lock
// everyone out, and one targeted account cannot be ground down from many
// addresses.
var (
	signupPerIP      = ratelimit.PerHour(10)
	signupPerEmail   = ratelimit.PerHour(3)
	loginPerIP       = ratelimit.PerMinute(10)
	loginPerEmail    = ratelimit.PerHour(20)
	mailPerEmail     = ratelimit.PerHour(3)
	mailPerIP        = ratelimit.PerHour(10)
	tokenPerIP       = ratelimit.PerMinute(20)
	changePwdPerUser = ratelimit.PerHour(10)

	// apiPerUser is the ceiling on every authenticated control-plane request,
	// applied as middleware rather than per handler so a route cannot be added
	// without one. It is set well above what a dashboard needs, because its job
	// is to bound the damage a session can do -- each request costs a session
	// join -- not to pace normal use. Handlers that guard something specific
	// keep their own tighter bucket on top: changePwdPerUser is the example.
	apiPerUser = ratelimit.PerMinute(120)
)

const maxEmailLen = 254

// emailKey derives a rate-limit key from an address. The address is hashed so
// that Redis never holds a list of who uses the service, which would be a
// standing privacy problem in a store the spec treats as expendable.
func emailKey(email string) string {
	sum := sha256.Sum256([]byte(store.NormalizeEmail(email)))
	return hex.EncodeToString(sum[:16])
}

// validateEmail applies the checks a mail server would, plus a length cap.
func validateEmail(raw string) (string, bool) {
	email := store.NormalizeEmail(raw)
	if email == "" || len(email) > maxEmailLen {
		return "", false
	}
	// ParseAddress accepts display-name forms such as `A <a@b.c>`; requiring
	// the parsed address to equal the input rejects those, so the stored
	// address is exactly what was submitted.
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", false
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", false
	}
	return email, true
}

type signupRequest struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	WorkspaceName string `json:"workspace_name"`
}

type statusResponse struct {
	Status string `json:"status"`
}

// handleSignup registers a user, creates their workspace and makes them its
// admin.
//
// The response is identical whether or not the address was already
// registered. Signup is unauthenticated, so a distinguishable answer would
// turn it into a free membership oracle for any address an attacker cares
// about. The real owner of an already-registered address learns nothing new,
// and the attacker learns nothing at all.
func (d Deps) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	email, ok := validateEmail(req.Email)
	if !ok {
		invalidField(w, "email", "Provide a valid email address.")
		return
	}
	// The password is judged by HashPassword below, which is the only place the
	// rules live. It runs before the rate limit is spent, as the email check
	// does, so a malformed request does not cost the caller budget.
	if err := auth.ValidatePassword(req.Password); err != nil {
		invalidField(w, "password", err.Error())
		return
	}

	if !d.allow(w, r, "signup:ip", d.Auth.RateLimitKey(r), signupPerIP) {
		return
	}
	if !d.allow(w, r, "signup:email", emailKey(email), signupPerEmail) {
		return
	}

	workspaceName := strings.TrimSpace(req.WorkspaceName)
	if workspaceName == "" {
		local, _, _ := strings.Cut(email, "@")
		workspaceName = local + " workspace"
	}
	if len(workspaceName) > 100 {
		invalidField(w, "workspace_name", "Workspace name must be at most 100 characters.")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		if auth.IsPasswordRejected(err) {
			invalidField(w, "password", err.Error())
			return
		}
		d.serverErrorf(w, r, err, "hash password")
		return
	}

	result, err := d.Store.CreateUserWithWorkspace(r.Context(), email, hash, workspaceName)
	switch {
	case errors.Is(err, store.ErrEmailTaken):
		// Answer as if it had worked, and send nothing: a notification here
		// would confirm the address to whoever triggered it.
		d.logger().InfoContext(r.Context(), "signup for an address already registered")
		writeJSON(w, http.StatusCreated, statusResponse{Status: "pending_verification"})
		return
	case err != nil:
		d.serverErrorf(w, r, err, "create user")
		return
	}

	// The id comes back from the insert and the address was validated twenty
	// lines above, so there is nothing to re-read the row for.
	if err := d.Auth.IssueVerification(r.Context(), result.UserID, email, d.Config.PublicBaseURL, d.Mailer); err != nil {
		// The account exists and the user can ask for another link, so a
		// delivery failure must not fail the signup.
		d.logger().ErrorContext(r.Context(), "send verification email",
			slog.String("error", err.Error()))
	}

	writeJSON(w, http.StatusCreated, statusResponse{Status: "pending_verification"})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin verifies a password and starts a session.
//
// Every failure answers with one message and one status. Separating "no such
// account" from "wrong password" would hand an attacker the first half of
// every credential pair for free.
func (d Deps) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email := store.NormalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		invalidCredentials(w)
		return
	}

	if !d.allow(w, r, "login:ip", d.Auth.RateLimitKey(r), loginPerIP) {
		return
	}
	if !d.allow(w, r, "login:email", emailKey(email), loginPerEmail) {
		return
	}

	user, err := d.Auth.Authenticate(r.Context(), email, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordMismatch) {
			invalidCredentials(w)
			return
		}
		d.serverErrorf(w, r, err, "authenticate")
		return
	}

	workspace, err := d.Store.DefaultWorkspace(r.Context(), user.ID)
	if err != nil {
		// A user with no membership cannot act anywhere, and the signup
		// transaction makes this unreachable, so it is a server-side fault
		// rather than a credential problem.
		d.serverErrorf(w, r, err, "resolve default workspace")
		return
	}

	token, session, err := d.Auth.CreateSession(r.Context(), user.ID, workspace.ID, r)
	if err != nil {
		d.serverErrorf(w, r, err, "create session")
		return
	}

	// A correct password clears that account's attempt bucket, so a user who
	// mistyped a few times is not then throttled.
	if err := d.Limiter.Reset(r.Context(), "login:email", emailKey(email)); err != nil {
		d.logger().WarnContext(r.Context(), "reset login bucket", slog.String("error", err.Error()))
	}

	d.Auth.SetSessionCookies(w, token)

	// Assembled from the three rows this handler already holds rather than read
	// back through ResolveSession. That call was an extra multi-table join for
	// an answer already in hand, and its failure path told a caller 500 while
	// signing them in -- a cookie set, a session live, and a response saying the
	// login did not happen.
	writeJSON(w, http.StatusOK, meResponse(&store.Identity{
		Session:         *session,
		Email:           user.Email,
		EmailVerifiedAt: user.EmailVerifiedAt,
		WorkspaceName:   workspace.Name,
	}))
}

func invalidCredentials(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, typeAuthentication, "invalid_credentials",
		"Email or password is incorrect.")
}

// handleLogout revokes the session behind the presented cookie. It is
// deliberately not behind RequireSession: logging out must work even with a
// session that has already expired, and it must never answer 401.
func (d Deps) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := d.Auth.RevokeSession(r.Context(), d.Auth.SessionTokenFromRequest(r)); err != nil {
		d.serverErrorf(w, r, err, "revoke session")
		return
	}
	d.Auth.ClearSessionCookies(w)
	writeJSON(w, http.StatusOK, statusResponse{Status: "signed_out"})
}

// handleLogoutAll ends every session the caller holds, which is what a user
// reaches for after losing a device.
func (d Deps) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		unauthorized(w, r)
		return
	}
	if _, err := d.Store.RevokeUserSessions(r.Context(), identity.Session.UserID, ""); err != nil {
		d.serverErrorf(w, r, err, "revoke all sessions")
		return
	}
	d.Auth.ClearSessionCookies(w)
	writeJSON(w, http.StatusOK, statusResponse{Status: "all_sessions_signed_out"})
}

type meBody struct {
	User struct {
		ID            string     `json:"id"`
		Email         string     `json:"email"`
		EmailVerified bool       `json:"email_verified"`
		VerifiedAt    *time.Time `json:"email_verified_at,omitempty"`
	} `json:"user"`
	Workspace struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"workspace"`
	Session struct {
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"session"`
}

func meResponse(identity *store.Identity) meBody {
	var body meBody
	body.User.ID = identity.Session.UserID
	body.User.Email = identity.Email
	body.User.EmailVerified = identity.EmailVerified()
	body.User.VerifiedAt = identity.EmailVerifiedAt
	body.Workspace.ID = identity.Session.WorkspaceID
	body.Workspace.Name = identity.WorkspaceName
	body.Session.ExpiresAt = identity.Session.ExpiresAt
	return body
}

// handleMe reports who the caller is. The dashboard uses it to decide what to
// show, and it is also how a client discovers whether its cookie is still
// good.
func (d Deps) handleMe(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		unauthorized(w, r)
		return
	}
	writeJSON(w, http.StatusOK, meResponse(identity))
}

type tokenRequest struct {
	Token string `json:"token"`
}

// handleVerifyEmail spends a verification token. It accepts the token from
// the query string, because that is what the emailed link carries, and from a
// JSON body for a dashboard that reads the token out of the URL itself.
func (d Deps) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	if !d.allow(w, r, "verify:ip", d.Auth.RateLimitKey(r), tokenPerIP) {
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" && r.Method == http.MethodPost {
		var req tokenRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		token = req.Token
	}

	if err := d.Auth.ConsumeVerification(r.Context(), token); err != nil {
		if errors.Is(err, auth.ErrInvalidToken) {
			badRequest(w, "invalid_token", "This link is invalid, already used or expired.")
			return
		}
		d.serverErrorf(w, r, err, "consume verification token")
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "email_verified"})
}

type emailRequest struct {
	Email string `json:"email"`
}

// handleResendVerification sends a fresh verification link. Like signup and
// forgot-password, it answers the same way for every address so it cannot be
// used to test whether one is registered.
func (d Deps) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email, ok := validateEmail(req.Email)
	if !ok {
		invalidField(w, "email", "Provide a valid email address.")
		return
	}
	if !d.allow(w, r, "mail:ip", d.Auth.RateLimitKey(r), mailPerIP) {
		return
	}
	if !d.allow(w, r, "mail:verify", emailKey(email), mailPerEmail) {
		return
	}

	user, err := d.Store.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Nothing to send; answer as if there were.
	case err != nil:
		d.serverErrorf(w, r, err, "look up user")
		return
	case user.EmailVerified():
		// Already verified; another link would be useless.
	default:
		if err := d.Auth.IssueVerification(r.Context(), user.ID, user.Email, d.Config.PublicBaseURL, d.Mailer); err != nil {
			d.serverErrorf(w, r, err, "send verification email")
			return
		}
	}
	writeJSON(w, http.StatusAccepted, statusResponse{Status: "verification_sent"})
}

// handleForgotPassword starts a password reset. It always answers 202: a
// different answer for an unknown address would let anyone enumerate accounts
// from an endpoint that needs no credentials at all.
func (d Deps) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email, ok := validateEmail(req.Email)
	if !ok {
		invalidField(w, "email", "Provide a valid email address.")
		return
	}
	if !d.allow(w, r, "mail:ip", d.Auth.RateLimitKey(r), mailPerIP) {
		return
	}
	if !d.allow(w, r, "mail:reset", emailKey(email), mailPerEmail) {
		return
	}

	user, err := d.Store.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Nothing to do.
	case err != nil:
		d.serverErrorf(w, r, err, "look up user")
		return
	default:
		if err := d.Auth.IssuePasswordReset(r.Context(), user.ID, user.Email, d.Config.PublicBaseURL, d.Mailer); err != nil {
			d.serverErrorf(w, r, err, "send reset email")
			return
		}
	}
	writeJSON(w, http.StatusAccepted, statusResponse{Status: "reset_requested"})
}

type resetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// handleResetPassword spends a reset token and sets a new password. The store
// revokes every session in the same transaction, so recovering an account
// also evicts whoever else was in it.
func (d Deps) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !d.allow(w, r, "reset:ip", d.Auth.RateLimitKey(r), tokenPerIP) {
		return
	}

	// The password is not pre-checked here. The service decides the order, and
	// it checks the token first, so a caller who does not hold a valid reset
	// link is never told which rules their proposed password broke.
	switch err := d.Auth.ResetPassword(r.Context(), req.Token, req.Password); {
	case errors.Is(err, auth.ErrInvalidToken):
		badRequest(w, "invalid_token", "This reset link is invalid, already used or expired.")
		return
	case auth.IsPasswordRejected(err):
		invalidField(w, "password", err.Error())
		return
	case err != nil:
		d.serverErrorf(w, r, err, "reset password")
		return
	}

	// The caller's own cookie, if any, was revoked along with the rest.
	d.Auth.ClearSessionCookies(w)
	writeJSON(w, http.StatusOK, statusResponse{Status: "password_reset"})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword replaces a signed-in user's password. The current
// password is required even though the session already proves possession of
// the cookie: it is what stops a borrowed, unlocked browser from taking the
// account over outright.
func (d Deps) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		unauthorized(w, r)
		return
	}
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !d.allow(w, r, "password:user", identity.Session.UserID, changePwdPerUser) {
		return
	}

	// As in reset, the rules are the service's to apply. It verifies the current
	// password first, so this handler cannot be the thing that decides whether a
	// caller with the wrong current password hears about it or about their new
	// one.
	err := d.Auth.ChangePassword(r.Context(),
		identity.Session.UserID, identity.Session.ID, req.CurrentPassword, req.NewPassword)
	switch {
	case errors.Is(err, auth.ErrPasswordMismatch):
		writeFieldError(w, http.StatusForbidden, typePermission, "invalid_credentials",
			"current_password", "Current password is incorrect.")
		return
	case auth.IsPasswordRejected(err):
		invalidField(w, "new_password", err.Error())
		return
	case err != nil:
		d.serverErrorf(w, r, err, "change password")
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{Status: "password_changed"})
}
