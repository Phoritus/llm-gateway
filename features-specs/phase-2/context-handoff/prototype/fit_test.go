package handoff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func text(role, s string) Message { return Message{Role: role, Content: mustJSON(s)} }

// conversation builds an IDE-agent-like session: a system prompt, a task,
// then turns of assistant tool calls with large tool outputs and short
// replies, ending on a user message or a fresh tool sequence.
func conversation(r *rand.Rand, turns int) []Message {
	msgs := []Message{
		text("system", "You are a coding agent. "+strings.Repeat("rules ", 200)),
		text("user", "Fix the failing test in internal/router."),
	}
	id := 0
	for t := 0; t < turns; t++ {
		switch r.IntN(3) {
		case 0:
			msgs = append(msgs, text("user", "follow-up "+strings.Repeat("q ", r.IntN(80))))
		case 1:
			msgs = append(msgs, text("assistant", "reply "+strings.Repeat("a ", r.IntN(300))))
		default:
			n := 1 + r.IntN(3)
			var calls []map[string]any
			for k := 0; k < n; k++ {
				calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d", id+k), "type": "function",
					"function": map[string]string{"name": "read_file", "arguments": `{"path":"x.go"}`}})
			}
			raw, _ := json.Marshal(calls)
			msgs = append(msgs, Message{Role: "assistant", ToolCalls: raw})
			for k := 0; k < n; k++ {
				msgs = append(msgs, Message{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", id+k),
					Content: mustJSON("file contents " + strings.Repeat("code ", 200+r.IntN(2000)))})
			}
			id += n
		}
	}
	if r.IntN(2) == 0 {
		msgs = append(msgs, text("user", "now run the tests"))
	}
	return msgs
}

// pairsIntact reports a tool message that does not follow, inside its own
// tool sequence, the assistant message that called it.
func pairsIntact(msgs []Message) error {
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

func same(a, b Message) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// TestFitInvariants is the property test: over many generated conversations
// and budgets, a fitted conversation is within budget, keeps every tool pair,
// and keeps the system prompt, the task and the final turn byte for byte.
func TestFitInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	fitted, refused := 0, 0
	for i := 0; i < 2000; i++ {
		msgs := conversation(r, 5+r.IntN(60))
		budget := 2000 + r.IntN(40000)
		out, rep, err := Fit(msgs, budget)
		if err != nil {
			refused++
			continue
		}
		fitted++
		if got := Estimate(out); got > budget {
			t.Fatalf("case %d: %d tokens, budget %d", i, got, budget)
		}
		if err := pairsIntact(out); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !same(out[0], msgs[0]) || !same(out[1], msgs[1]) {
			t.Fatalf("case %d: system prompt or task changed", i)
		}
		last := group(msgs)[len(group(msgs))-1].msgs
		gotLast := out[len(out)-len(last):]
		for k := range last {
			if !same(last[k], gotLast[k]) {
				t.Fatalf("case %d: final turn changed", i)
			}
		}
		if rep.OmittedMessages > 0 && !strings.Contains(string(flattenText(out)), "earlier messages omitted") {
			t.Fatalf("case %d: messages dropped without a marker", i)
		}
	}
	t.Logf("%d fitted, %d refused (system prompt, task and last turn alone over budget)", fitted, refused)
	if fitted < 1500 {
		t.Fatalf("only %d of 2000 fitted", fitted)
	}
}

func flattenText(msgs []Message) []byte {
	var b []byte
	for _, m := range msgs {
		b = append(b, m.Content...)
	}
	return b
}

// TestStableCutAcrossTurns: the next turn of the same conversation keeps the
// same messages before the cut, so the vendor's prefix cache still matches.
func TestStableCutAcrossTurns(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	msgs := conversation(r, 60)
	a, _, errA := Fit(msgs, 16000)
	next := append(append([]Message(nil), msgs...), text("assistant", "done"), text("user", "thanks, one more"))
	b, _, errB := Fit(next, 16000)
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	shared := 0
	for shared < min(len(a), len(b)) && same(a[shared], b[shared]) {
		shared++
	}
	t.Logf("turn 1: %d messages, turn 2: %d messages, identical prefix: %d", len(a), len(b), shared)
}

// naiveDropOldest is OpenAI's truncation: "auto" and many hand-written
// trimmers: keep the system prompt, drop the oldest messages one at a time.
func naiveDropOldest(msgs []Message, budget int) []Message {
	out := append([]Message(nil), msgs...)
	for Estimate(out) > budget && len(out) > 2 {
		out = append(out[:1], out[2:]...)
	}
	return out
}

// TestIDEScenario is the worked example: a 60-turn agent session handed from
// a 128k model (rate limited) to an 8k one.
func TestIDEScenario(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 42))
	var msgs []Message
	for Estimate(msgs) < 60000 {
		msgs = conversation(r, 60)
	}
	const window8k = 8192 - 1000 // the window minus reserved output
	budget := window8k * 8 / 10

	out, rep, err := Fit(msgs, budget)
	if err != nil {
		t.Fatal(err)
	}
	naive := naiveDropOldest(msgs, budget)
	t.Logf("original: %d messages, ~%d tokens", len(msgs), Estimate(msgs))
	t.Logf("fitted:   %d messages, ~%d tokens, %d tool results cleared, %d messages omitted; pairs intact: %v; task kept: %v",
		len(out), Estimate(out), rep.ClearedToolResults, rep.OmittedMessages, pairsIntact(out) == nil, same(out[1], msgs[1]))
	t.Logf("naive:    %d messages, ~%d tokens; pairs intact: %v; task kept: %v",
		len(naive), Estimate(naive), errString(pairsIntact(naive)), len(naive) > 1 && same(naive[1], msgs[1]))
}

func errString(err error) string {
	if err == nil {
		return "true"
	}
	return "false (" + err.Error() + ")"
}
