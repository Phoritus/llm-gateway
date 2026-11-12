package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

type gatewayKeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func gatewayKeyViewOf(k store.GatewayKey) gatewayKeyView {
	return gatewayKeyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt,
		LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt}
}

func (d Deps) handleListGatewayKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := d.Store.ListGatewayKeys(r.Context(), auth.WorkspaceID(r.Context()))
	if err != nil {
		d.serverErrorf(w, r, err, "list gateway keys")
		return
	}
	views := make([]gatewayKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, gatewayKeyViewOf(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": views})
}

func (d Deps) handleCreateGatewayKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name, ok := parseNameField(w, req.Name)
	if !ok {
		return
	}

	token, key, err := d.GatewayKeys.Mint(r.Context(), auth.WorkspaceID(r.Context()), name)
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
		}{gatewayKeyViewOf(*key), token})
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
