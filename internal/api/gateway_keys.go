package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/budget"
	"llm-gateway/internal/store"
)

type gatewayKeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	budgetFields
	SpentThisMonthUSD *float64 `json:"spent_this_month_usd"`
}

func gatewayKeyViewOf(k store.GatewayKey, spent *float64) gatewayKeyView {
	return gatewayKeyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt,
		LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt, budgetFields: budgetFieldsOf(k.Budget),
		SpentThisMonthUSD: spent}
}

func keyAccounts(keys ...store.GatewayKey) []budget.Account {
	out := make([]budget.Account, len(keys))
	for i, k := range keys {
		out[i] = budget.Account{Scope: budget.ScopeKey, ID: k.ID}
	}
	return out
}

func (d Deps) handleListGatewayKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := d.Store.ListGatewayKeys(r.Context(), auth.WorkspaceID(r.Context()))
	if err != nil {
		d.serverErrorf(w, r, err, "list gateway keys")
		return
	}
	spent := d.spent(r.Context(), keyAccounts(keys...))
	views := make([]gatewayKeyView, 0, len(keys))
	for i, k := range keys {
		views = append(views, gatewayKeyViewOf(k, spent[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": views})
}

func (d Deps) handleCreateGatewayKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		budgetFields
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name, ok := parseNameField(w, req.Name)
	if !ok {
		return
	}
	limit, ok := req.budget(w)
	if !ok {
		return
	}

	token, key, err := d.GatewayKeys.Mint(r.Context(), auth.WorkspaceID(r.Context()), name, limit)
	switch {
	case errors.Is(err, store.ErrLimitReached):
		writeError(w, http.StatusConflict, typeInvalidRequest, "gateway_key_limit_reached",
			"This workspace has reached its limit of 50 active gateway keys. Revoke one to create another.")
	case err != nil:
		d.serverErrorf(w, r, err, "create gateway key")
	default:
		writeJSON(w, http.StatusCreated, struct {
			gatewayKeyView
			Key string `json:"key"`
		}{gatewayKeyViewOf(*key, d.spent(r.Context(), keyAccounts(*key))[0]), token})
	}
}

// handlePutGatewayKeyBudget is PUT /api/gateway-keys/{id}/budget, which
// replaces the key's budget. Other instances apply it within the gateway key
// cache's second.
func (d Deps) handlePutGatewayKeyBudget(w http.ResponseWriter, r *http.Request) {
	var req budgetFields
	if !decodeJSON(w, r, &req) {
		return
	}
	limit, ok := req.budget(w)
	if !ok {
		return
	}
	key, err := d.Store.SetGatewayKeyBudget(r.Context(), auth.WorkspaceID(r.Context()), chi.URLParam(r, "id"), limit)
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, r)
	case err != nil:
		d.serverErrorf(w, r, err, "set gateway key budget")
	default:
		writeJSON(w, http.StatusOK, gatewayKeyViewOf(*key, d.spent(r.Context(), keyAccounts(*key))[0]))
	}
}

func (d Deps) handleRevokeGatewayKey(w http.ResponseWriter, r *http.Request) {
	err := d.GatewayKeys.Revoke(r.Context(), auth.WorkspaceID(r.Context()), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		notFound(w, r)
	case err != nil:
		d.serverErrorf(w, r, err, "revoke gateway key")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
