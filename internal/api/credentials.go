package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

const (
	maxName      = 100
	minSecretLen = 20
	maxSecretLen = 512
)

type credentialView struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Name      string    `json:"name"`
	Last4     string    `json:"last4"`
	CreatedAt time.Time `json:"created_at"`
}

func viewOf(c store.Credential) credentialView {
	return credentialView{ID: c.ID, Provider: string(c.Provider), Name: c.Name, Last4: c.Last4, CreatedAt: c.CreatedAt}
}

func (d Deps) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := d.Credentials.List(r.Context(), auth.WorkspaceID(r.Context()))
	if err != nil {
		d.serverErrorf(w, r, err, "list credentials")
		return
	}
	views := make([]credentialView, 0, len(creds))
	for _, c := range creds {
		views = append(views, viewOf(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": views})
}

func (d Deps) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Secret   string `json:"secret"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	vendor, ok := parseVendorField(w, req.Provider, d.Config.IsProduction())
	if !ok {
		return
	}
	name, ok := parseNameField(w, req.Name)
	if !ok {
		return
	}
	secret := strings.TrimSpace(req.Secret)
	if !validSecret(secret) {
		invalidField(w, "secret", "secret must be 20 to 512 printable ASCII characters with no spaces.")
		return
	}

	cred, err := d.Credentials.Create(r.Context(), auth.WorkspaceID(r.Context()),
		store.NewCredential{Provider: vendor, Name: name, Secret: secret})
	switch {
	case errors.Is(err, store.ErrLimitReached):
		writeError(w, http.StatusConflict, typeInvalidRequest, "credential_limit_reached",
			"This workspace has reached its limit of 50 provider credentials. Delete one to add another.")
	case err != nil:
		d.serverErrorf(w, r, err, "create credential")
	default:
		writeJSON(w, http.StatusCreated, viewOf(*cred))
	}
}

func (d Deps) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	err := d.Credentials.Delete(r.Context(), auth.WorkspaceID(r.Context()), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, r)
	case errors.Is(err, store.ErrCredentialInUse):
		writeError(w, http.StatusConflict, typeInvalidRequest, "credential_in_use",
			"A route still uses this credential. Remove it from every route first.")
	case err != nil:
		d.serverErrorf(w, r, err, "delete credential")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

const keyTestTimeout = 10 * time.Second

type keyTestResult struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// handleTestCredential asks the credential's provider whether it accepts the
// key. The answer is 200 either way: the test ran, and its outcome is the
// body. The upstream's own message is not passed on, as on /v1.
func (d Deps) handleTestCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := d.Credentials.Unseal(r.Context(), auth.WorkspaceID(r.Context()), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		notFound(w, r)
		return
	}
	if err != nil {
		d.serverErrorf(w, r, err, "unseal credential")
		return
	}
	adapter, ok := d.Providers[cred.Vendor]
	if !ok {
		writeJSON(w, http.StatusOK, keyTestResult{Code: "provider_unavailable",
			Message: "This provider is not available on this gateway yet."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), keyTestTimeout)
	defer cancel()
	err = adapter.CheckKey(ctx, provider.Credential{APIKey: cred.Secret})
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		d.logger().InfoContext(r.Context(), "credential test failed",
			slog.String("request_id", telemetry.RequestIDFrom(r.Context())),
			slog.String("vendor", string(cred.Vendor)), slog.String("error", err.Error()))
	}
	writeJSON(w, http.StatusOK, keyTestOutcome(err))
}

func keyTestOutcome(err error) keyTestResult {
	switch {
	case err == nil:
		return keyTestResult{OK: true}
	case errors.Is(err, provider.ErrAuth), errors.Is(err, provider.ErrModelUnavailable):
		return keyTestResult{Code: "key_rejected", Message: "The provider rejected this key."}
	case errors.Is(err, provider.ErrQuota):
		return keyTestResult{Code: "quota_exhausted", Message: "The provider accepted this key, but its account has no quota or credit left."}
	case errors.Is(err, provider.ErrRateLimited):
		return keyTestResult{Code: "rate_limited", Message: "The provider is rate limiting this key. Try the test again later."}
	case errors.Is(err, provider.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return keyTestResult{Code: "provider_timeout", Message: "The provider did not answer in time."}
	default:
		return keyTestResult{Code: "provider_error", Message: "The provider could not check this key."}
	}
}

func parseVendorField(w http.ResponseWriter, raw string, production bool) (provider.Vendor, bool) {
	usable := func(v provider.Vendor) bool { return !production || !v.DevelopmentOnly() }
	if vendor, ok := provider.ParseVendor(raw); ok && usable(vendor) {
		return vendor, true
	}
	var names []string
	for _, v := range provider.Vendors() {
		if usable(v) {
			names = append(names, string(v))
		}
	}
	invalidField(w, "provider", "provider must be one of: "+strings.Join(names, ", ")+".")
	return "", false
}

func parseNameField(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if utf8.RuneCountInString(name) > maxName || strings.ContainsFunc(name, unicode.IsControl) {
		invalidField(w, "name", "name must be at most 100 characters, with no control characters.")
		return "", false
	}
	return name, true
}

func validSecret(s string) bool {
	return visibleASCII(s, minSecretLen, maxSecretLen)
}

func visibleASCII(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
