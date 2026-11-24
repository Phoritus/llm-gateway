package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
)

// Provider is one upstream model API. Streaming is the only chat call: a
// non-streaming request is the same stream put together by Assemble, which the
// exact cache needs anyway, so an adapter implements one chat method rather
// than two that must agree.
type Provider interface {
	// ChatStream starts a generation. An error here, or from the stream's
	// first Next, happens before anything reaches the client, so the caller
	// may still try another target.
	ChatStream(ctx context.Context, req *ChatRequest, cred Credential) (Stream, error)

	// CheckKey asks the upstream whether it accepts the credential, without
	// generating anything.
	CheckKey(ctx context.Context, cred Credential) error
}

// Stream yields a generation's chunks in order. Next returns io.EOF after the
// last chunk; any other error ends the stream. Close releases the upstream
// connection and is safe to call at any point, including after an error.
type Stream interface {
	Next() (*ChatChunk, error)
	Close() error
}

// Credential is what an adapter authenticates upstream with. It is passed per
// call, not bound at construction, because one adapter and its connection pool
// serve every workspace's keys.
type Credential struct {
	APIKey  string
	BaseURL string
}

// ChatRequest is the OpenAI chat completions request. The polymorphic parts --
// stop, tools, tool_choice, and message content that may be a string or an
// array of parts -- stay raw, so they pass through to an OpenAI-compatible
// upstream unchanged without this package modelling OpenAI's whole schema.
//
// The fields the gateway reads are modelled; every other top-level field (n,
// seed, response_format, penalties, logprobs, and whatever OpenAI adds next)
// is kept in Extra and written back out, so a passthrough forwards it.
type ChatRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *StreamOptions  `json:"stream_options,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	Stop          json.RawMessage `json:"stop,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	User          string          `json:"user,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

type chatRequestFields ChatRequest

var modelledFields = func() []string {
	t := reflect.TypeFor[chatRequestFields]()
	var names []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}()

func (r *ChatRequest) UnmarshalJSON(b []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	if err := json.Unmarshal(b, (*chatRequestFields)(r)); err != nil {
		return err
	}
	// encoding/json matches field names case-insensitively, so "MODEL" was
	// decoded into Model and must not also travel as an extra.
	for key := range all {
		for _, name := range modelledFields {
			if strings.EqualFold(key, name) {
				delete(all, key)
				break
			}
		}
	}
	r.Extra = nil
	if len(all) > 0 {
		r.Extra = all
	}
	return nil
}

func (r ChatRequest) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(chatRequestFields(r))
	if err != nil || len(r.Extra) == 0 {
		return b, err
	}
	var modelled map[string]json.RawMessage
	if err := json.Unmarshal(b, &modelled); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(r.Extra)+len(modelled))
	for k, v := range r.Extra {
		out[k] = v
	}
	for k, v := range modelled {
		out[k] = v
	}
	return json.Marshal(out)
}

// StreamOptions asks for a final chunk that carries usage.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Text returns the message's text: the content when it is a string, or its
// text parts joined when it is an array of parts. Anything else is "".
func (m Message) Text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var out string
	for _, p := range parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// ChatChunk is one chat.completion.chunk.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is a chat.completion: the non-streaming answer.
type ChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []ResponseItem `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

type ResponseItem struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason *string         `json:"finish_reason"`
}

type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one call in an assistant message. In a stream it arrives in
// pieces: a ToolCallDelta per chunk, keyed by Index, with Arguments split
// across them.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
