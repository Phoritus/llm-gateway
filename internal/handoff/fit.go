package handoff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"llm-gateway/internal/provider"
)

// ErrDoesNotFit is a request whose pinned messages alone are over the budget.
var ErrDoesNotFit = errors.New("the system messages, tools and final turn alone are over the window")

// Estimate is the request's prompt in the gateway's tokens: a quarter of the
// bytes its messages and tools encode to, as the rate limiter counts.
func Estimate(req *provider.ChatRequest) int {
	return estimate(req.Messages) + quarter(len(req.Tools))
}

func estimate(msgs []provider.Message) int {
	b, _ := json.Marshal(msgs)
	return quarter(len(b))
}

func quarter(n int) int { return (n + 3) / 4 }

// DefaultRatio is how many upstream tokens one estimated token is taken to be
// until an upstream has said. bytes/4 runs under the truth for code and for
// scripts other than Latin, so it errs high.
const DefaultRatio = 1.25

// Budget is how many estimated tokens a request may take to fit window
// upstream tokens: the window less what the answer is given, nine tenths of
// that for the estimate's error, at ratio upstream tokens per estimated one.
// The answer is given the request's max_tokens, up to half the window, or a
// quarter of the window up to 1000 when it set none.
func Budget(window int, maxTokens *int, ratio float64) int {
	reserved := min(1000, window/4)
	if maxTokens != nil && *maxTokens > 0 {
		reserved = min(*maxTokens, window/2)
	}
	return int(float64(window-reserved) * 0.9 / ratio)
}

// Report says what Fit changed, in counts only: never any of the text.
type Report struct {
	ClearedToolResults int
	OmittedMessages    int
}

// Changed reports whether Fit changed anything.
func (r Report) Changed() bool { return r.ClearedToolResults > 0 || r.OmittedMessages > 0 }

const keepToolResults = 3

var placeholder = quote("[context handoff: tool result cleared to fit the model's window]")

type unit struct {
	msgs []provider.Message
	size []int
}

func (u unit) pinned() bool {
	r := u.msgs[0].Role
	return r == "system" || r == "developer"
}

func group(msgs []provider.Message) []unit {
	var units []unit
	for i := 0; i < len(msgs); i++ {
		u := unit{msgs: []provider.Message{msgs[i]}}
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			for i+1 < len(msgs) && msgs[i+1].Role == "tool" {
				i++
				u.msgs = append(u.msgs, msgs[i])
			}
		}
		for _, m := range u.msgs {
			u.size = append(u.size, encodedLen(m))
		}
		units = append(units, u)
	}
	return units
}

func encodedLen(m provider.Message) int {
	b, _ := json.Marshal(m)
	return len(b)
}

func marker(omitted int) provider.Message {
	return provider.Message{Role: "user", Content: quote(fmt.Sprintf(
		"[context handoff: %d earlier messages omitted to fit the model's window]", omitted))}
}

// Fit returns req with its conversation changed as little as it takes to fit
// budget estimated tokens, or ErrDoesNotFit. A request that fits is returned
// as it came.
//
// Pinned, byte for byte: every system and developer message, the tools and
// the final unit (the last message, or the last tool_calls message with its
// results). Everything else may go, in this order: older tool results are
// cleared, oldest first, keeping the newest few; then whole units are dropped,
// oldest first, in steps of a quarter of what may be dropped, so the cut stays
// put across the turns of one conversation and an upstream's prompt cache keeps
// matching. A step that leaves less than half the budget used is given back a
// unit at a time, newest first, while it still fits. One marker message stands
// where the dropped units were. A tool result is never parted from its call,
// and no message is cut inside.
func Fit(req *provider.ChatRequest, budget int) (*provider.ChatRequest, Report, error) {
	var rep Report
	if Estimate(req) <= budget {
		return req, rep, nil
	}
	budget -= quarter(len(req.Tools))
	units := group(req.Messages)
	last := units[len(units)-1]
	history := units[:len(units)-1]

	omittedBy := func(dropUnits int) (omitted int) {
		for i, gone := 0, 0; gone < dropUnits; i++ {
			if !history[i].pinned() {
				omitted += len(history[i].msgs)
				gone++
			}
		}
		return omitted
	}
	tokens := func(dropUnits int) int {
		n, count := 0, 0
		add := func(size int) { n += size; count++ }
		if omitted := omittedBy(dropUnits); omitted > 0 {
			add(encodedLen(marker(omitted)))
		}
		gone := 0
		for _, u := range history {
			if gone < dropUnits && !u.pinned() {
				gone++
				continue
			}
			for _, size := range u.size {
				add(size)
			}
		}
		for _, size := range last.size {
			add(size)
		}
		return quarter(2 + n + count - 1)
	}

	var tools [][2]int
	for ui, u := range history {
		for mi, m := range u.msgs {
			if !u.pinned() && m.Role == "tool" {
				tools = append(tools, [2]int{ui, mi})
			}
		}
	}
	for i := 0; i < len(tools)-keepToolResults && tokens(0) > budget; i++ {
		ui, mi := tools[i][0], tools[i][1]
		u := history[ui]
		u.msgs = append([]provider.Message(nil), u.msgs...)
		u.size = append([]int(nil), u.size...)
		u.msgs[mi].Content = placeholder
		u.size[mi] = encodedLen(u.msgs[mi])
		history[ui] = u
		rep.ClearedToolResults++
	}

	droppable := 0
	for _, u := range history {
		if !u.pinned() {
			droppable++
		}
	}
	step := max(1, droppable/4)
	dropUnits := 0
	for tokens(dropUnits) > budget && dropUnits < droppable {
		dropUnits = min(droppable, dropUnits+step)
	}
	for floor := max(0, dropUnits-step); dropUnits > floor && tokens(dropUnits) < budget/2 && tokens(dropUnits-1) <= budget; {
		dropUnits--
	}
	if tokens(dropUnits) > budget {
		return nil, rep, ErrDoesNotFit
	}

	rep.OmittedMessages = omittedBy(dropUnits)
	var out []provider.Message
	gone := 0
	for _, u := range history {
		if gone < dropUnits && !u.pinned() {
			if gone == 0 {
				out = append(out, marker(rep.OmittedMessages))
			}
			gone++
			continue
		}
		out = append(out, u.msgs...)
	}
	fitted := *req
	fitted.Messages = append(out, last.msgs...)
	rep.ClearedToolResults = 0
	for _, m := range fitted.Messages {
		if bytes.Equal(m.Content, placeholder) {
			rep.ClearedToolResults++
		}
	}
	return &fitted, rep, nil
}

func quote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
