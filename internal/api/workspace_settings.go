package api

import (
	"net/http"
	"strconv"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

const maxPromptLogRetentionDays = 30

type settingsView struct {
	CacheEnabled           bool `json:"cache_enabled"`
	SemanticCacheEnabled   bool `json:"semantic_cache_enabled"`
	PromptLoggingEnabled   bool `json:"prompt_logging_enabled"`
	PromptLogRetentionDays int  `json:"prompt_log_retention_days"`
	budgetFields
}

func settingsViewOf(s store.WorkspaceSettings) settingsView {
	return settingsView{
		CacheEnabled: !s.CacheOff, SemanticCacheEnabled: s.SemanticCache,
		PromptLoggingEnabled: s.PromptLogging, PromptLogRetentionDays: s.LogRetentionDays(),
		budgetFields: budgetFieldsOf(s.Budget),
	}
}

func defaultSettingsView() settingsView {
	return settingsViewOf(store.WorkspaceSettings{})
}

func (v settingsView) settings(b *store.Budget) store.WorkspaceSettings {
	return store.WorkspaceSettings{
		CacheOff: !v.CacheEnabled, SemanticCache: v.SemanticCacheEnabled,
		PromptLogging: v.PromptLoggingEnabled, PromptLogRetentionDays: v.PromptLogRetentionDays,
		Budget: b,
	}
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
	if in.PromptLogRetentionDays == 0 {
		in.PromptLogRetentionDays = store.DefaultPromptLogRetentionDays
	}
	if in.PromptLogRetentionDays < 1 || in.PromptLogRetentionDays > maxPromptLogRetentionDays {
		invalidField(w, "prompt_log_retention_days",
			"prompt_log_retention_days must be from 1 to "+strconv.Itoa(maxPromptLogRetentionDays)+".")
		return
	}
	limit, ok := in.budget(w)
	if !ok {
		return
	}
	if err := d.Store.SetWorkspaceSettings(r.Context(), auth.WorkspaceID(r.Context()), in.settings(limit)); err != nil {
		d.serverErrorf(w, r, err, "write workspace settings")
		return
	}
	writeJSON(w, http.StatusOK, in)
}
