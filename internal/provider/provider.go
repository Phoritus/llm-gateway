package provider

import (
	"context"
	"encoding/json"
)

// Provider is one upstream model API. Streaming is the only call: a
// non-streaming request is the same stream put together by Assemble, which the
// exact cache needs anyway, so an adapter implements one method rather than
// two that must agree.
type Provider interface {
	// ChatStream starts a generation. An error here, or from the stream's
	// first Next, happens before anything reaches the client, so the caller
	// may still try another target.
	ChatStream(ctx context.Context, req *ChatRequest, cred Credential) (Stream, error)
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
	APIKey string
}

// ChatRequest is the OpenAI chat completions request. The polymorphic parts --
// stop, tools, tool_choice, and message content that may be a string or an
// array of parts -- stay raw, so they pass through to an OpenAI-compatible
// upstream unchanged without this package modelling OpenAI's whole schema.
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
	Role    string `json:"role"`
	Content string `json:"content"`
}
