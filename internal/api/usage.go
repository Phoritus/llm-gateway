package api

import (
	"net/http"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

const (
	usageDateLayout  = "2006-01-02"
	defaultUsageDays = 30
	maxUsageDays     = 366
	recentUsageLimit = 50
)

type usageDayView struct {
	Day              string `json:"day"`
	Route            string `json:"route"`
	Vendor           string `json:"vendor"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	AvgLatencyMs     int64  `json:"avg_latency_ms"`
	CacheHits        int64  `json:"cache_hits"`
}

type usageEventView struct {
	CreatedAt        time.Time     `json:"created_at"`
	Route            string        `json:"route"`
	Vendor           string        `json:"vendor"`
	Model            string        `json:"model"`
	Status           int           `json:"status"`
	Outcome          store.Outcome `json:"outcome"`
	Attempts         int           `json:"attempts"`
	PromptTokens     int           `json:"prompt_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
	LatencyMs        int           `json:"latency_ms"`
	TTFTMs           int           `json:"ttft_ms"`
	Cached           bool          `json:"cached"`
}

// handleUsage is GET /api/usage: the daily rollup for a range of UTC days, and
// the workspace's latest requests.
func (d Deps) handleUsage(w http.ResponseWriter, r *http.Request) {
	from, to, ok := usageRange(w, r)
	if !ok {
		return
	}
	route := r.URL.Query().Get("route")
	if route != "" && !validAlias(route) {
		invalidField(w, "route", "route must be a route's alias: "+aliasRule+".")
		return
	}
	workspaceID := auth.WorkspaceID(r.Context())
	days, err := d.Store.Daily(r.Context(), workspaceID, from, to, route)
	if err != nil {
		d.serverErrorf(w, r, err, "read usage rollup")
		return
	}
	recent, err := d.Store.Recent(r.Context(), workspaceID, recentUsageLimit)
	if err != nil {
		d.serverErrorf(w, r, err, "read recent usage")
		return
	}

	dayViews := make([]usageDayView, len(days))
	for i, u := range days {
		dayViews[i] = usageDayView{
			Day: u.Day.Format(usageDateLayout), Route: u.RouteAlias, Vendor: u.Vendor, Model: u.Model,
			Requests: u.Requests, Errors: u.Errors, PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
			CacheHits: u.CacheHits,
		}
		if u.Requests > 0 {
			dayViews[i].AvgLatencyMs = u.LatencyMsSum / u.Requests
		}
	}
	eventViews := make([]usageEventView, len(recent))
	for i, e := range recent {
		eventViews[i] = usageEventView{
			CreatedAt: e.CreatedAt, Route: e.RouteAlias, Vendor: e.Vendor, Model: e.Model, Status: e.Status,
			Outcome: e.Outcome, Attempts: e.Attempts, PromptTokens: e.PromptTokens,
			CompletionTokens: e.CompletionTokens, LatencyMs: e.LatencyMs, TTFTMs: e.TTFTMs, Cached: e.Cached,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": dayViews, "recent": eventViews})
}

// usageRange reads from and to as UTC dates and returns a range of at most
// 366 days. With neither, it is the 30 days ending today. With to alone, the
// 30 days ending on it. With from alone, the 30 days starting on it, cut
// short at today when today falls inside them.
func usageRange(w http.ResponseWriter, r *http.Request) (from, to time.Time, ok bool) {
	q := r.URL.Query()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	parse := func(param string) (time.Time, bool, bool) {
		raw := q.Get(param)
		if raw == "" {
			return time.Time{}, false, true
		}
		t, err := time.Parse(usageDateLayout, raw)
		if err != nil {
			invalidField(w, param, param+" must be a date as YYYY-MM-DD.")
			return time.Time{}, false, false
		}
		return t, true, true
	}
	from, hasFrom, ok := parse("from")
	if !ok {
		return from, to, false
	}
	to, hasTo, ok := parse("to")
	if !ok {
		return from, to, false
	}
	switch {
	case !hasTo && !hasFrom:
		to = today
		from = to.AddDate(0, 0, 1-defaultUsageDays)
	case !hasTo:
		to = from.AddDate(0, 0, defaultUsageDays-1)
		if today.Before(to) && !today.Before(from) {
			to = today
		}
	case !hasFrom:
		from = to.AddDate(0, 0, 1-defaultUsageDays)
	}
	if to.Before(from) {
		invalidField(w, "to", "to must not be before from.")
		return from, to, false
	}
	if int(to.Sub(from).Hours()/24)+1 > maxUsageDays {
		invalidField(w, "to", "The range may cover at most 366 days.")
		return from, to, false
	}
	return from, to, true
}
