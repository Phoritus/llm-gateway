package provider

import (
	"encoding/json"
	"strings"
)

// maxChoices bounds the n a response may carry. OpenAI caps n at 128.
const maxChoices = 128

const maxToolCalls = 128

// Assembler builds a ChatResponse from a stream's chunks as they pass. It is
// fed chunk by chunk rather than handed a Stream so that the same chunks can
// go to a client at the same time: a non-streaming request assembles them
// silently, and the exact cache will assemble a streaming one while it is
// being sent.
type Assembler struct {
	resp    ChatResponse
	choices []*choiceParts
}

type choiceParts struct {
	content strings.Builder
	calls   []*callParts
}

type callParts struct {
	call      ToolCall
	arguments strings.Builder
}

func (a *Assembler) Add(c *ChatChunk) {
	if a.resp.ID == "" {
		a.resp.ID, a.resp.Created, a.resp.Model = c.ID, c.Created, c.Model
	}
	if c.Usage != nil {
		u := *c.Usage
		a.resp.Usage = &u
	}
	for _, ch := range c.Choices {
		// The index comes from upstream. One out of range is dropped rather
		// than trusted to size a slice.
		if ch.Index < 0 || ch.Index >= maxChoices {
			continue
		}
		for len(a.resp.Choices) <= ch.Index {
			a.resp.Choices = append(a.resp.Choices, ResponseItem{
				Index:   len(a.resp.Choices),
				Message: ResponseMessage{Role: "assistant"},
			})
			a.choices = append(a.choices, new(choiceParts))
		}
		item := &a.resp.Choices[ch.Index]
		parts := a.choices[ch.Index]
		if ch.Delta.Role != "" {
			item.Message.Role = ch.Delta.Role
		}
		parts.content.WriteString(ch.Delta.Content)
		parts.addToolCalls(ch.Delta.ToolCalls)
		if ch.FinishReason != nil {
			item.FinishReason = ch.FinishReason
		}
	}
}

func (p *choiceParts) addToolCalls(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var deltas []ToolCallDelta
	if json.Unmarshal(raw, &deltas) != nil {
		return
	}
	for _, d := range deltas {
		if d.Index < 0 || d.Index >= maxToolCalls {
			continue
		}
		for len(p.calls) <= d.Index {
			p.calls = append(p.calls, new(callParts))
		}
		c := p.calls[d.Index]
		if d.ID != "" {
			c.call.ID = d.ID
		}
		if d.Type != "" {
			c.call.Type = d.Type
		}
		if d.Function.Name != "" {
			c.call.Function.Name = d.Function.Name
		}
		c.arguments.WriteString(d.Function.Arguments)
	}
}

// Response returns the assembled chat.completion.
func (a *Assembler) Response() *ChatResponse {
	out := a.resp
	out.Object = "chat.completion"
	out.Choices = make([]ResponseItem, len(a.resp.Choices))
	for i, item := range a.resp.Choices {
		parts := a.choices[i]
		item.Message.Content = parts.content.String()
		for _, c := range parts.calls {
			if c.call.ID == "" && c.call.Function.Name == "" && c.arguments.Len() == 0 {
				continue
			}
			call := c.call
			if call.Type == "" {
				call.Type = "function"
			}
			call.Function.Arguments = c.arguments.String()
			item.Message.ToolCalls = append(item.Message.ToolCalls, call)
		}
		out.Choices[i] = item
	}
	return &out
}
