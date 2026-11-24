package handoff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"llm-gateway/internal/provider"
)

func text(role, s string) provider.Message { return provider.Message{Role: role, Content: quote(s)} }

func request(msgs []provider.Message) *provider.ChatRequest {
	return &provider.ChatRequest{Model: "m", Messages: msgs}
}

func conversation(r *rand.Rand, turns int) []provider.Message {
	msgs := []provider.Message{
		text("system", "You are a coding agent. "+strings.Repeat("rules ", 200)),
		text("user", "Fix the failing test in internal/router."),
	}
	id := 0
	for range turns {
		switch r.IntN(7) {
		case 0, 1:
			msgs = append(msgs, text("user", "follow-up "+strings.Repeat("q ", r.IntN(80))))
		case 2, 3:
			msgs = append(msgs, text("assistant", "reply "+strings.Repeat("a ", r.IntN(300))))
		case 4:
			msgs = append(msgs, text("developer", "mode: careful"))
		default:
			n := 1 + r.IntN(3)
			var calls []map[string]any
			for k := range n {
				calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d", id+k), "type": "function",
					"function": map[string]string{"name": "read_file", "arguments": `{"path":"x.go"}`}})
			}
			raw, _ := json.Marshal(calls)
			msgs = append(msgs, provider.Message{Role: "assistant", ToolCalls: raw})
			for k := range n {
				msgs = append(msgs, provider.Message{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", id+k),
					Content: quote("file contents " + strings.Repeat("code ", 200+r.IntN(2000)))})
			}
			id += n
		}
	}
	if r.IntN(2) == 0 {
		msgs = append(msgs, text("user", "now run the tests"))
	}
	return msgs
}

func pairsIntact(msgs []provider.Message) error {
	open := map[string]bool{}
	for i, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			open = map[string]bool{}
			var calls []struct{ ID string }
			_ = json.Unmarshal(m.ToolCalls, &calls)
			for _, c := range calls {
				open[c.ID] = true
			}
		case m.Role == "tool":
			if !open[m.ToolCallID] {
				return fmt.Errorf("message %d: tool result %s has no preceding call", i, m.ToolCallID)
			}
		default:
			open = map[string]bool{}
		}
	}
	return nil
}

func encoded(m provider.Message) []byte {
	b, _ := json.Marshal(m)
	return b
}

func same(a, b provider.Message) bool { return bytes.Equal(encoded(a), encoded(b)) }

func pinnedOf(msgs []provider.Message) [][]byte {
	var out [][]byte
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			out = append(out, encoded(m))
		}
	}
	return out
}

func TestFitInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	fitted, refused, taskDropped := 0, 0, 0
	for i := range 1000 {
		msgs := conversation(r, 5+r.IntN(60))
		budget := 2000 + r.IntN(40000)
		req := request(msgs)
		out, rep, err := Fit(req, budget)
		if errors.Is(err, ErrDoesNotFit) {
			refused++
			continue
		}
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		fitted++
		got := out.Messages
		if e := Estimate(out); e > budget {
			t.Fatalf("case %d: %d tokens, budget %d", i, e, budget)
		}
		if err := pairsIntact(got); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		want, have := pinnedOf(msgs), pinnedOf(got)
		if len(want) != len(have) {
			t.Fatalf("case %d: %d system and developer messages kept of %d", i, len(have), len(want))
		}
		for k := range want {
			if !bytes.Equal(want[k], have[k]) {
				t.Fatalf("case %d: system or developer message %d changed", i, k)
			}
		}
		units := group(msgs)
		last := units[len(units)-1].msgs
		for k := range last {
			if !same(last[k], got[len(got)-len(last)+k]) {
				t.Fatalf("case %d: final unit changed", i)
			}
		}
		if rep.OmittedMessages > 0 && !strings.Contains(contents(got), "earlier messages omitted") {
			t.Fatalf("case %d: messages dropped without a marker", i)
		}
		if rep.Changed() != (len(got) != len(msgs) || rep.ClearedToolResults > 0) {
			t.Fatalf("case %d: report %+v does not match the change", i, rep)
		}
		if !same(got[1], msgs[1]) {
			taskDropped++
		}
		if &req.Messages[0] == &got[0] && rep.Changed() {
			t.Fatalf("case %d: fitted in place", i)
		}
	}
	t.Logf("%d fitted, %d refused, %d of the fitted lost their first user message", fitted, refused, taskDropped)
	if fitted < 750 {
		t.Fatalf("only %d of 1000 fitted", fitted)
	}
	if taskDropped == 0 {
		t.Fatal("the first user message was never dropped: it is still pinned")
	}
}

func contents(msgs []provider.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.Write(m.Content)
	}
	return b.String()
}

func TestARequestThatFitsIsReturnedAsItCame(t *testing.T) {
	req := request([]provider.Message{text("user", "hi")})
	out, rep, err := Fit(req, 1000)
	if err != nil || out != req || rep.Changed() {
		t.Fatalf("Fit = %p, %+v, %v; want the request itself, unchanged", out, rep, err)
	}
}

func TestPinnedMessagesOverTheBudgetDoNotFit(t *testing.T) {
	req := request([]provider.Message{
		text("system", strings.Repeat("s", 4000)),
		text("user", "old"),
		text("user", "now"),
	})
	if _, _, err := Fit(req, 500); !errors.Is(err, ErrDoesNotFit) {
		t.Fatalf("err = %v, want ErrDoesNotFit", err)
	}
}

func TestTheCutStaysPutAcrossTurns(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	msgs := conversation(r, 60)
	a, _, errA := Fit(request(msgs), 16000)
	next := append(append([]provider.Message(nil), msgs...), text("assistant", "done"), text("user", "thanks, one more"))
	b, _, errB := Fit(request(next), 16000)
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	shared := 0
	for shared < min(len(a.Messages), len(b.Messages)) && same(a.Messages[shared], b.Messages[shared]) {
		shared++
	}
	if shared < len(a.Messages)/2 {
		t.Errorf("turn 1 sent %d messages, turn 2 %d, sharing a prefix of only %d", len(a.Messages), len(b.Messages), shared)
	}
}

func TestALongAgentSessionFitsAnEightKWindow(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 42))
	var msgs []provider.Message
	for estimate(msgs) < 60000 {
		msgs = conversation(r, 60)
	}
	budget := Budget(8192, nil, DefaultRatio)
	out, rep, err := Fit(request(msgs), budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := pairsIntact(out.Messages); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d messages (~%d tokens) to %d (~%d): %d tool results cleared, %d messages omitted",
		len(msgs), estimate(msgs), len(out.Messages), Estimate(out), rep.ClearedToolResults, rep.OmittedMessages)
}

func TestToolsCountAgainstTheBudgetAndAreKept(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"` + strings.Repeat("t", 400) + `"}}]`)
	req := &provider.ChatRequest{Messages: []provider.Message{
		text("user", strings.Repeat("old ", 200)),
		text("user", "now"),
	}, Tools: tools}
	out, _, err := Fit(req, 150)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Tools, tools) || Estimate(out) > 150 {
		t.Fatalf("tools changed or over budget: %d tokens", Estimate(out))
	}
}

func TestBudgetReservesTheAnswer(t *testing.T) {
	big := 100000
	for _, tc := range []struct {
		window    int
		maxTokens *int
		ratio     float64
		want      int
	}{
		{8192, nil, 1, 6472},
		{2000, nil, 1, 1350},
		{8192, &big, 1, 3686},
		{8192, nil, 2, 3236},
	} {
		if got := Budget(tc.window, tc.maxTokens, tc.ratio); got != tc.want {
			t.Errorf("Budget(%d, %v, %v) = %d, want %d", tc.window, tc.maxTokens, tc.ratio, got, tc.want)
		}
	}
}
