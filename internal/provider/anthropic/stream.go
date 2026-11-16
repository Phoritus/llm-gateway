package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/sse"
)

// streamEvent is the union of the Messages stream's events; Type says which
// fields were sent.
type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage usage  `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage usage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// toolCallDelta omits what a continuation piece must not repeat, which
// provider.ToolCallDelta cannot: it has no omitempty.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type stream struct {
	ctx    context.Context
	body   io.ReadCloser
	events *sse.Reader

	includeUsage bool

	id           string
	model        string
	created      int64
	promptTokens int

	// toolIndex maps an Anthropic content block index to the OpenAI tool call
	// index, since only tool_use blocks count as tool calls.
	toolIndex map[int]int

	queue []*provider.ChatChunk
	// end is how the stream finishes once the queue drains: io.EOF for a clean
	// end, otherwise the failure.
	end error
}

func (s *stream) Next() (*provider.ChatChunk, error) {
	for {
		if len(s.queue) > 0 {
			c := s.queue[0]
			s.queue = s.queue[1:]
			return c, nil
		}
		if s.end != nil {
			return nil, s.end
		}
		raw, err := s.events.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.end = &provider.Error{Kind: provider.ErrUpstream, Detail: "stream ended before it finished"}
			} else {
				s.end = callError(s.ctx, err)
			}
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			s.end = &provider.Error{Kind: provider.ErrUpstream, Detail: "malformed event: " + truncate(raw.Data)}
			continue
		}
		s.handle(&ev)
	}
}

func (s *stream) handle(ev *streamEvent) {
	switch ev.Type {
	case "message_start":
		s.id, s.model, s.created = ev.Message.ID, ev.Message.Model, time.Now().Unix()
		u := ev.Message.Usage
		s.promptTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		s.emit(provider.Delta{Role: "assistant"}, nil)
	case "content_block_start":
		if ev.ContentBlock.Type != "tool_use" {
			return
		}
		idx := len(s.toolIndex)
		s.toolIndex[ev.Index] = idx
		call := toolCallDelta{Index: idx, ID: ev.ContentBlock.ID, Type: "function"}
		call.Function.Name = ev.ContentBlock.Name
		s.emitToolCall(call)
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				s.emit(provider.Delta{Content: ev.Delta.Text}, nil)
			}
		case "input_json_delta":
			idx, ok := s.toolIndex[ev.Index]
			if !ok || ev.Delta.PartialJSON == "" {
				return
			}
			call := toolCallDelta{Index: idx}
			call.Function.Arguments = ev.Delta.PartialJSON
			s.emitToolCall(call)
		}
	case "message_delta":
		reason := finishReason(ev.Delta.StopReason)
		s.emit(provider.Delta{}, &reason)
		if s.includeUsage {
			completion := ev.Usage.OutputTokens
			s.queue = append(s.queue, &provider.ChatChunk{
				ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
				Choices: []provider.ChunkChoice{},
				Usage: &provider.Usage{
					PromptTokens: s.promptTokens, CompletionTokens: completion, TotalTokens: s.promptTokens + completion,
				},
			})
		}
	case "message_stop":
		s.end = io.EOF
	case "error":
		s.end = classify(0, ev.Error.Type, ev.Error.Message)
	}
	// content_block_stop and ping carry nothing to forward.
}

func (s *stream) emit(d provider.Delta, finish *string) {
	s.queue = append(s.queue, &provider.ChatChunk{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []provider.ChunkChoice{{Index: 0, Delta: d, FinishReason: finish}},
	})
}

func (s *stream) emitToolCall(call toolCallDelta) {
	raw, _ := json.Marshal([]toolCallDelta{call})
	s.emit(provider.Delta{ToolCalls: raw}, nil)
}

func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

func (s *stream) Close() error {
	s.end = io.EOF
	s.queue = nil
	return s.body.Close()
}
