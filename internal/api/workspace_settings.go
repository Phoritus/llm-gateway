package api

import (
	"net/http"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

type settingsView struct {
	CacheEnabled         bool `json:"cache_enabled"`
	SemanticCacheEnabled bool `json:"semantic_cache_enabled"`
}

func settingsViewOf(s store.WorkspaceSettings) settingsView {
	return settingsView{CacheEnabled: !s.CacheOff, SemanticCacheEnabled: s.SemanticCache}
}

func defaultSettingsView() settingsView {
	return settingsViewOf(store.WorkspaceSettings{})
}

func (v settingsView) settings() store.WorkspaceSettings {
	return store.WorkspaceSettings{CacheOff: !v.CacheEnabled, SemanticCache: v.SemanticCacheEnabled}
}

func (d Deps) handleGetWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	s, err := d.Store.WorkspaceSettings(r.Context(), auth.WorkspaceID(r.Context()))
	if err != nil {
		d.serverErrorf(w, r, err, "read workspace settings")
		return
	}
	writeJSON(w, http.StatusOK, settingsViewOf(s))
}

func (d Deps) handlePutWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	in := defaultSettingsView()
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := d.Store.SetWorkspaceSettings(r.Context(), auth.WorkspaceID(r.Context()), in.settings()); err != nil {
		d.serverErrorf(w, r, err, "write workspace settings")
		return
	}
	writeJSON(w, http.StatusOK, in)
}
