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

func TestATargetWhoseWindowCannotHoldThePinnedMessagesIsStillCalled(t *testing.T) {
	h := newHarness(t)
	a, b := target("a", provider.VendorMock, "s"), target("b", provider.VendorMock, "t")
	h.knowWindow(a, 1200)
	h.knowWindow(b, 300)
	req := &provider.ChatRequest{Messages: []provider.Message{say("system", strings.Repeat("s", 4000)), say("user", "hi")}}
	res, called, sent, err := h.route(store.HandoffTrim, req, map[string]func(*provider.ChatRequest) error{
		"b": windowed(300),
	}, a, b)
	if err != nil {
		t.Fatalf("the guessed window was wrong and a can answer: %v", err)
	}
	wantCalled(t, called, "a")
	if sent[0] != req || !res.Handoff.Unfittable {
		t.Errorf("a was not sent the request as it came, or handoff %+v not marked unfittable", res.Handoff)
	}
	if res.Attempts() != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts())
	}

	h = newHarness(t)
	h.knowWindow(b, 300)
	_, called, _, err = h.route(store.HandoffTrim, req, map[string]func(*provider.ChatRequest) error{"b": windowed(300)}, b)
	wantCalled(t, called, "b")
	if w, _ := provider.ContextLimit(err); !errors.Is(err, provider.ErrContextTooLong) || w != 300 {
		t.Errorf("err = %v with window %d, want the window the target stated", err, w)
	}
}

// TestARefitThatCannotShrinkIsNotSent: an upstream over its window because of
// max_tokens leaves nothing for fitting to cut, so the next target is tried
// instead of the same request twice.
func TestARefitThatCannotShrinkIsNotSent(t *testing.T) {
	h := newHarness(t)
	limited, a, b := target("r", provider.VendorMock, "r"), target("a", provider.VendorMock, "s"), target("b", provider.VendorMock, "t")
	maxTokens := 8900
	req := &provider.ChatRequest{MaxTokens: &maxTokens, Messages: []provider.Message{say("user", "hi")}}
	_, called, _, err := h.route(store.HandoffTrim, req, map[string]func(*provider.ChatRequest) error{
		"r": func(*provider.ChatRequest) error { return failure(provider.ErrRateLimited, 0) },
		"a": func(*provider.ChatRequest) error {
			return provider.ContextTooLong("This model's maximum context length is 8192 tokens. However, you requested 8910 tokens (10 in the messages, 8900 in the completion).", "")
		},
	}, limited, a, b)
	if err != nil {
		t.Fatal(err)
	}
	wantCalled(t, called, "r", "a", "b")
}

func TestARatioOneVendorShowedIsNotUsedForAnother(t *testing.T) {
	h := newHarness(t)
	a, b := target("a", provider.VendorGroq, "x"), target("b", provider.VendorMock, "y")
	h.knowWindow(b, 16000)
	req := longChat(6000)
	_, called, sent, err := h.route(store.HandoffTrim, req, map[string]func(*provider.ChatRequest) error{
		"a": func(r *provider.ChatRequest) error {
			return provider.ContextTooLong(fmt.Sprintf("maximum context length is 2000 tokens. However, your messages resulted in %d tokens.", 4*handoff.Estimate(r)), "")
		},
	}, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if last := sent[len(sent)-1]; called[len(called)-1] != "b" || last != req {
		t.Errorf("called %v; b was sent %d of %d tokens, want the request as it came", called, handoff.Estimate(last), handoff.Estimate(req))
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
