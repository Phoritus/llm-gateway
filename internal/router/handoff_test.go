package router

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/handoff"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

func say(role, s string) provider.Message {
	return provider.Message{Role: role, Content: []byte(strconv.Quote(s))}
}

func longChat(tokens int) *provider.ChatRequest {
	msgs := []provider.Message{say("system", "be brief")}
	for len(msgs) < 40 {
		msgs = append(msgs, say("user", strings.Repeat("q", tokens/10)), say("assistant", strings.Repeat("a", tokens/10)))
	}
	return &provider.ChatRequest{Model: "alias", Messages: append(msgs, say("user", "and now?"))}
}

func windowed(window int) func(*provider.ChatRequest) error {
	return func(req *provider.ChatRequest) error {
		if n := handoff.Estimate(req); n > window {
			return provider.ContextTooLong(fmt.Sprintf("maximum context length is %d tokens. However, your messages resulted in %d tokens.", window, n), "")
		}
		return nil
	}
}

func (h *harness) route(mode store.HandoffMode, req *provider.ChatRequest, answer map[string]func(*provider.ChatRequest) error, targets ...store.ResolvedTarget) (Result, []string, []*provider.ChatRequest, error) {
	h.t.Helper()
	var called []string
	var sent []*provider.ChatRequest
	res, err := h.r.Run(context.Background(), &store.ResolvedRoute{ID: "r", MaxHops: 3, ContextHandoff: mode, Targets: targets}, req,
		func(_ context.Context, t store.ResolvedTarget, req *provider.ChatRequest) (provider.Stream, *provider.ChatChunk, error) {
			called = append(called, t.CredentialID)
			sent = append(sent, req)
			if f := answer[t.CredentialID]; f != nil {
				if err := f(req); err != nil {
					return nil, nil, err
				}
			}
			return &fakeStream{}, &provider.ChatChunk{ID: t.CredentialID}, nil
		})
	h.r.Wait()
	return res, called, sent, err
}

func (h *harness) knowWindow(t store.ResolvedTarget, n int) {
	h.t.Helper()
	if err := h.r.state.shared.Set(context.Background(), []Entry{{Key: window.key(t), Value: strconv.Itoa(n), TTL: time.Hour}}); err != nil {
		h.t.Fatal(err)
	}
}

func TestATargetKnownToBeTooSmallIsTriedLast(t *testing.T) {
	small, big := target("small", provider.VendorMock, "s"), target("big", provider.VendorMock, "b")
	for _, tc := range []struct {
		mode      store.HandoffMode
		reordered bool
	}{{store.HandoffReorder, true}, {store.HandoffTrim, true}, {store.HandoffOff, false}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			h := newHarness(t)
			h.knowWindow(small, 1000)
			req := longChat(8000)
			res, called, sent, err := h.route(tc.mode, req, nil, small, big)
			if err != nil {
				t.Fatal(err)
			}
			want := "small"
			if tc.reordered {
				want = "big"
			}
			wantCalled(t, called, want)
			if res.Handoff.Reordered != tc.reordered || sent[0] != req {
				t.Errorf("handoff = %+v, or the request was changed", res.Handoff)
			}
			wantAction := store.HandoffAction("")
			if tc.reordered {
				wantAction = store.HandoffReordered
			}
			if res.Handoff.Action() != wantAction {
				t.Errorf("action = %q, want %q", res.Handoff.Action(), wantAction)
			}
		})
	}
}

func TestTrimFitsTheConversationToAWindowTheErrorStated(t *testing.T) {
	big, small := target("big", provider.VendorMock, "b"), target("small", provider.VendorMock, "s")
	answer := map[string]func(*provider.ChatRequest) error{
		"big":   func(*provider.ChatRequest) error { return failure(provider.ErrRateLimited, 0) },
		"small": windowed(2000),
	}
	req := longChat(8000)

	h := newHarness(t)
	_, called, _, err := h.route(store.HandoffReorder, req, answer, big, small)
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("reorder: err = %v, want rate limited", err)
	}
	wantCalled(t, called, "big", "small")

	h = newHarness(t)
	res, called, sent, err := h.route(store.HandoffTrim, req, answer, big, small)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	wantCalled(t, called, "big", "small", "small")
	wantOutcomes(t, res.Steps, OutcomeRateLimited, OutcomeContextTooLong, OutcomeOK)
	if !res.Steps[2].Trimmed || res.Steps[1].Trimmed {
		t.Errorf("trimmed flags = %v, %v; want only the retry", res.Steps[1].Trimmed, res.Steps[2].Trimmed)
	}
	if sent[1] != req || handoff.Estimate(sent[2]) > 2000 {
		t.Errorf("first call not sent as it came, or the retry (%d tokens) over the window", handoff.Estimate(sent[2]))
	}
	if res.Handoff.Action() != store.HandoffTrimmed || res.Handoff.Fitted.OmittedMessages == 0 {
		t.Errorf("handoff = %+v", res.Handoff)
	}

	res, called, _, err = h.route(store.HandoffTrim, req, answer, big, small)
	if err != nil {
		t.Fatal(err)
	}
	wantCalled(t, called, "small")
	wantOutcomes(t, res.Steps, OutcomeSkipped, OutcomeOK)
	if res.Handoff.Reordered || !res.Steps[1].Trimmed {
		t.Errorf("second request: handoff %+v, steps %+v", res.Handoff, res.Steps)
	}
}

func TestAnErrorThatStatesNoWindowIsNotRetried(t *testing.T) {
	h := newHarness(t)
	a := target("a", provider.VendorMock, "s")
	res, called, _, err := h.route(store.HandoffTrim, longChat(8000), map[string]func(*provider.ChatRequest) error{
		"a": func(*provider.ChatRequest) error { return failure(provider.ErrContextTooLong, 0) },
	}, a)
	wantCalled(t, called, "a")
	if !errors.Is(err, provider.ErrContextTooLong) {
		t.Fatalf("err = %v", err)
	}
	if w, _ := provider.ContextLimit(err); w != 0 || res.Handoff.Action() != "" {
		t.Errorf("window %d, handoff %+v", w, res.Handoff)
	}
}

func TestWhatCanNeverBeDroppedOverTheWindowIsRefusedWithoutACall(t *testing.T) {
	h := newHarness(t)
	a, b := target("a", provider.VendorMock, "s"), target("b", provider.VendorMock, "t")
	h.knowWindow(a, 300)
	h.knowWindow(b, 200)
	req := &provider.ChatRequest{Messages: []provider.Message{say("system", strings.Repeat("s", 4000)), say("user", "hi")}}
	res, called, _, err := h.route(store.HandoffTrim, req, nil, a, b)
	wantCalled(t, called)
	wantOutcomes(t, res.Steps, OutcomeContextTooLong, OutcomeContextTooLong)
	if !res.Handoff.TargetRefused {
		t.Error("not marked refused")
	}
	if w, _ := provider.ContextLimit(err); !errors.Is(err, provider.ErrContextTooLong) || w != 300 {
		t.Errorf("err = %v with window %d, want context too long stating the largest window, 300", err, w)
	}
}

func TestAWindowStatedInAnErrorIsLearnedInEveryMode(t *testing.T) {
	h := newHarness(t)
	a := target("a", provider.VendorMock, "s")
	_, _, _, _ = h.route(store.HandoffOff, longChat(8000), map[string]func(*provider.ChatRequest) error{"a": windowed(1500)}, a)
	got, err := h.r.state.shared.Get(context.Background(), []string{window.key(a)})
	if err != nil || got[0] != "1500" {
		t.Fatalf("stored window = %q, %v; want 1500", got, err)
	}
}
