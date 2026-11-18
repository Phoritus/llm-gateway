// Package handoff is a prototype of ticket 06's deterministic fitter: make an
// OpenAI chat conversation fit a smaller context window without breaking it.
package handoff

import (
	"encoding/json"
	"fmt"
)

// Message mirrors provider.Message.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Estimate is the gateway's bytes/4, over the encoded message.
func Estimate(msgs []Message) int {
	b, _ := json.Marshal(msgs)
	return (len(b) + 3) / 4
}

// Report says what Fit did, in counts only: no prompt text.
type Report struct {
	ClearedToolResults int
	OmittedMessages    int
}

const keepToolResults = 3

var placeholder = mustJSON("[context handoff: tool result cleared to fit the model's window]")

// unit is one indivisible piece of history: a single message, or an
// assistant tool_calls message together with every tool message answering it.
type unit struct{ msgs []Message }

func group(msgs []Message) []unit {
	var units []unit
	for i := 0; i < len(msgs); i++ {
		u := unit{msgs: []Message{msgs[i]}}
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			for i+1 < len(msgs) && msgs[i+1].Role == "tool" {
				i++
				u.msgs = append(u.msgs, msgs[i])
			}
		}
		units = append(units, u)
	}
	return units
}

func flatten(units []unit) []Message {
	var out []Message
	for _, u := range units {
		out = append(out, u.msgs...)
	}
	return out
}

// Fit returns msgs changed as little as it takes to fit budget tokens, or an
// error when what must be kept does not fit. Kept untouched: every system and
// developer message, the first user message and the final unit (the last user
// message, or the last tool_calls message with its results).
func Fit(msgs []Message, budget int) ([]Message, Report, error) {
	var rep Report
	if Estimate(msgs) <= budget {
		return msgs, rep, nil
	}
	units := group(msgs)

	// Head: leading system/developer units and the first user message.
	head := 0
	for head < len(units) && isSystem(units[head]) {
		head++
	}
	if head < len(units) && units[head].msgs[0].Role == "user" {
		head++
	}
	tail := len(units) - 1
	if tail < head {
		return nil, rep, fmt.Errorf("nothing to drop")
	}
	middle := append([]unit(nil), units[head:tail]...)
	build := func(omitted int) []Message {
		out := flatten(units[:head])
		if omitted > 0 {
			out = append(out, Message{Role: "user", Content: mustJSON(fmt.Sprintf(
				"[context handoff: %d earlier messages omitted to fit the model's window]", omitted))})
		}
		out = append(out, flatten(middle)...)
		return append(out, units[tail].msgs...)
	}

	// Step 1: clear older tool results, oldest first, keeping the newest few.
	var toolIdx [][2]int
	for ui, u := range middle {
		for mi, m := range u.msgs {
			if m.Role == "tool" {
				toolIdx = append(toolIdx, [2]int{ui, mi})
			}
		}
	}
	for i := 0; i < len(toolIdx)-keepToolResults && Estimate(build(0)) > budget; i++ {
		ui, mi := toolIdx[i][0], toolIdx[i][1]
		m := middle[ui].msgs[mi]
		m.Content = placeholder
		cp := append([]Message(nil), middle[ui].msgs...)
		cp[mi] = m
		middle[ui].msgs = cp
		rep.ClearedToolResults++
	}

	// Step 2: drop whole middle units, oldest first, in steps of a quarter of
	// the middle so the cut point stays put across turns.
	step := max(1, len(middle)/4)
	omitted := 0
	for Estimate(build(omitted)) > budget && len(middle) > 0 {
		n := min(step, len(middle))
		for _, u := range middle[:n] {
			omitted += len(u.msgs)
		}
		middle = middle[n:]
	}
	out := build(omitted)
	rep.OmittedMessages = omitted
	if Estimate(out) > budget {
		return nil, rep, fmt.Errorf("the system prompt, first and last turns alone need %d tokens, over %d", Estimate(out), budget)
	}
	return out, rep, nil
}

func isSystem(u unit) bool {
	r := u.msgs[0].Role
	return r == "system" || r == "developer"
}

func mustJSON(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
