package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

const (
	defaultLogLimit = 20
	maxLogLimit     = 50
	onDisable       = "kept until retention; DELETE /api/logs removes them now, but not the log of a request that ended in the last few seconds, which is still being written"
)

type loggingView struct {
	Enabled       bool   `json:"enabled"`
	RetentionDays int    `json:"retention_days"`
	OnDisable     string `json:"on_disable"`
}

type logView struct {
	ID                string        `json:"id"`
	CreatedAt         time.Time     `json:"created_at"`
	KeyID             string        `json:"key_id"`
	Route             string        `json:"route"`
	Vendor            string        `json:"vendor"`
	Model             string        `json:"model"`
	Status            int           `json:"status"`
	Outcome           store.Outcome `json:"outcome"`
	Request           string        `json:"request"`
	Response          string        `json:"response"`
	RequestTruncated  bool          `json:"request_truncated"`
	ResponseTruncated bool          `json:"response_truncated"`
}

type logsPage struct {
	Logging    loggingView `json:"logging"`
	Logs       []logView   `json:"logs"`
	NextCursor *string     `json:"next_cursor"`
}

func (d Deps) handleListLogs(w http.ResponseWriter, r *http.Request) {
	q, ok := logQuery(w, r)
	if !ok {
		return
	}
	workspaceID := auth.WorkspaceID(r.Context())
	settings, err := d.Store.WorkspaceSettings(r.Context(), workspaceID)
	if err != nil {
		d.serverErrorf(w, r, err, "read workspace settings")
		return
	}
	limit := q.Limit
	q.Limit++
	logs, err := d.Store.RequestLogs(r.Context(), workspaceID, q)
	if err != nil {
		d.serverErrorf(w, r, err, "read request logs")
		return
	}

	page := logsPage{
		Logging: loggingView{Enabled: settings.PromptLogging, RetentionDays: settings.LogRetentionDays(), OnDisable: onDisable},
		Logs:    make([]logView, 0, min(len(logs), limit)),
	}
	if len(logs) > limit {
		logs = logs[:limit]
		last := logs[limit-1]
		next := encodeCursor(store.LogCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		page.NextCursor = &next
	}
	for _, l := range logs {
		page.Logs = append(page.Logs, logView{
			ID: l.ID, CreatedAt: l.CreatedAt, KeyID: l.KeyID, Route: l.RouteAlias, Vendor: l.Vendor, Model: l.Model,
			Status: l.Status, Outcome: l.Outcome, Request: l.Request, Response: l.Response,
			RequestTruncated: l.RequestTruncated, ResponseTruncated: l.ResponseTruncated,
		})
	}
	writeJSONUnescaped(w, http.StatusOK, page)
}

func (d Deps) handleDeleteLogs(w http.ResponseWriter, r *http.Request) {
	if _, err := d.Store.DeleteRequestLogs(r.Context(), auth.WorkspaceID(r.Context())); err != nil {
		d.serverErrorf(w, r, err, "delete request logs")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func logQuery(w http.ResponseWriter, r *http.Request) (store.LogQuery, bool) {
	params := r.URL.Query()
	q := store.LogQuery{Route: params.Get("route"), Limit: defaultLogLimit}
	if q.Route != "" && !validAlias(q.Route) {
		invalidField(w, "route", "route must be a route's alias: "+aliasRule+".")
		return q, false
	}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		raw := params.Get(p.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			invalidField(w, p.name, p.name+" must be a time in RFC 3339 form, as 2026-11-24T09:00:00Z.")
			return q, false
		}
		*p.dst = t
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.To.After(q.From) {
		invalidField(w, "to", "to must be after from.")
		return q, false
	}
	if raw := params.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLogLimit {
			invalidField(w, "limit", "limit must be a whole number from 1 to "+strconv.Itoa(maxLogLimit)+".")
			return q, false
		}
		q.Limit = n
	}
	if raw := params.Get("cursor"); raw != "" {
		c, ok := decodeCursor(raw)
		if !ok {
			invalidField(w, "cursor", "cursor must be a next_cursor this endpoint returned.")
			return q, false
		}
		q.After = &c
	}
	return q, true
}

func encodeCursor(c store.LogCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

func decodeCursor(raw string) (store.LogCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return store.LogCursor{}, false
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return store.LogCursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return store.LogCursor{}, false
	}
	if _, err := uuid.Parse(id); err != nil {
		return store.LogCursor{}, false
	}
	return store.LogCursor{CreatedAt: t, ID: id}, true
}
