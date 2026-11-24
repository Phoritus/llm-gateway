package router

import (
	"context"
	"strconv"
	"time"

	"llm-gateway/internal/handoff"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

const windowTTL = 7 * 24 * time.Hour

// Another credential's account may be on another tier with another window.
var window = subject{"ctxwin:", credentialModel.id}

// Handoff is what context handoff did to one request.
type Handoff struct {
	// Reordered is set when targets were moved behind others because their
	// known window is too small for the request.
	Reordered bool
	// Fitted is what was cut from the request the answering target was sent.
	Fitted handoff.Report
	// TargetRefused is set when a target was not called because even the messages
	// fitting never drops would not fit its window.
	TargetRefused bool
}

// Action is the one name the request is recorded under.
func (h Handoff) Action() store.HandoffAction {
	switch {
	case h.Fitted.Changed():
		return store.HandoffTrimmed
	case h.Reordered:
		return store.HandoffReordered
	}
	return ""
}

func (v view) windowOf(t store.ResolvedTarget) int {
	if n, err := strconv.Atoi(v[window.key(t)]); err == nil && n > 0 {
		return n
	}
	n, _ := handoff.Window(t.Vendor, t.Model)
	return n
}

func fitsFirst(targets []store.ResolvedTarget, known view, req *provider.ChatRequest) ([]store.ResolvedTarget, bool) {
	tokens := handoff.Estimate(req)
	var fit, small []store.ResolvedTarget
	for _, t := range targets {
		if w := known.windowOf(t); w > 0 && tokens > handoff.Budget(w, req.MaxTokens, handoff.DefaultRatio) {
			small = append(small, t)
		} else {
			fit = append(fit, t)
		}
	}
	ordered := append(fit, small...)
	for i := range targets {
		if ordered[i] != targets[i] {
			return ordered, true
		}
	}
	return targets, false
}

func (r *Router) learnWindow(ctx context.Context, t store.ResolvedTarget, n int, known view) {
	key, value := window.key(t), strconv.Itoa(n)
	if known[key] == value {
		return
	}
	known[key] = value
	_ = r.recent.Set(ctx, []Entry{{Key: key, Value: value, TTL: copyTTL}})
	r.background(ctx, func(ctx context.Context) {
		r.state.Set(ctx, []Entry{{Key: key, Value: value, TTL: windowTTL}})
	})
}

const minRatio, maxRatio = 0.5, 4

func ratio(requested int, sent *provider.ChatRequest) float64 {
	est := handoff.Estimate(sent)
	if requested <= 0 || est <= 0 {
		return 0
	}
	return min(maxRatio, max(minRatio, float64(requested)/float64(est)))
}
