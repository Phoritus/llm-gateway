package provider

import "strings"

// maxChoices bounds the n a response may carry. OpenAI caps n at 128.
const maxChoices = 128

// Assembler builds a ChatResponse from a stream's chunks as they pass. It is
// fed chunk by chunk rather than handed a Stream so that the same chunks can
// go to a client at the same time: a non-streaming request assembles them
// silently, and the exact cache will assemble a streaming one while it is
// being sent.
//
// Tool-call deltas are not merged yet; they arrive with the first adapter
// that can emit them.
type Assembler struct {
	resp    ChatResponse
	content []*strings.Builder
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
			a.content = append(a.content, new(strings.Builder))
		}
		item := &a.resp.Choices[ch.Index]
		if ch.Delta.Role != "" {
			item.Message.Role = ch.Delta.Role
		}
		a.content[ch.Index].WriteString(ch.Delta.Content)
		if ch.FinishReason != nil {
			item.FinishReason = ch.FinishReason
		}
	}
}

// Response returns the assembled chat.completion.
func (a *Assembler) Response() *ChatResponse {
	out := a.resp
	out.Object = "chat.completion"
	out.Choices = make([]ResponseItem, len(a.resp.Choices))
	for i, item := range a.resp.Choices {
		item.Message.Content = a.content[i].String()
		out.Choices[i] = item
	}
	return &out
}
