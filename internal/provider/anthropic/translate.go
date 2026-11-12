package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"llm-gateway/internal/provider"
)

// defaultMaxTokens stands in when the client sent no limit: Anthropic requires
// one, and every current Claude model allows at least this many.
const defaultMaxTokens = 32000

type messagesRequest struct {
	Model         string        `json:"model"`
	MaxTokens     int           `json:"max_tokens"`
	System        []block       `json:"system,omitempty"`
	Messages      []message     `json:"messages"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Stream        bool          `json:"stream"`
	Tools         []tool        `json:"tools,omitempty"`
	ToolChoice    *toolChoice   `json:"tool_choice,omitempty"`
	Metadata      *metadata     `json:"metadata,omitempty"`
	OutputConfig  *outputConfig `json:"output_config,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is every content block kind in one struct; Type says which fields apply.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Strict      bool            `json:"strict,omitempty"`
}

type toolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type metadata struct {
	UserID string `json:"user_id"`
}

type outputConfig struct {
	Format json.RawMessage `json:"format"`
}

func invalid(format string, args ...any) error {
	return &provider.Error{Kind: provider.ErrInvalidRequest, Detail: fmt.Sprintf(format, args...)}
}

func translate(req *provider.ChatRequest) (*messagesRequest, error) {
	out := &messagesRequest{Model: req.Model, Stream: true}

	for i, m := range req.Messages {
		var (
			role   string
			blocks []block
			err    error
		)
		switch m.Role {
		case "system", "developer":
			if text := m.Text(); text != "" {
				out.System = append(out.System, block{Type: "text", Text: text})
			}
			continue
		case "user":
			role = "user"
			blocks, err = userBlocks(m)
		case "assistant":
			role = "assistant"
			blocks, err = assistantBlocks(m)
		case "tool":
			role = "user"
			blocks = []block{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Text()}}
		default:
			err = invalid("message %d has unsupported role %q", i, m.Role)
		}
		if err != nil {
			return nil, err
		}
		if len(blocks) == 0 {
			continue
		}
		// Anthropic requires alternating roles, and parallel tool results must
		// share one user turn.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
		} else {
			out.Messages = append(out.Messages, message{Role: role, Content: blocks})
		}
	}
	if len(out.Messages) == 0 {
		return nil, invalid("no messages besides system")
	}

	var err error
	if out.MaxTokens, err = maxTokens(req); err != nil {
		return nil, err
	}
	if out.StopSequences, err = stopSequences(req.Stop); err != nil {
		return nil, err
	}
	// Temperature and top_p are not forwarded: every current Claude model
	// refuses non-default sampling values and API version 2026-10-21 refuses the
	// fields outright, while OpenAI clients often send them by default.

	if out.Tools, err = convertTools(req.Tools); err != nil {
		return nil, err
	}
	if out.ToolChoice, err = convertToolChoice(req.ToolChoice); err != nil {
		return nil, err
	}
	if len(out.Tools) > 0 {
		var parallel bool
		if raw, ok := req.Extra["parallel_tool_calls"]; ok && json.Unmarshal(raw, &parallel) == nil && !parallel {
			if out.ToolChoice == nil {
				out.ToolChoice = &toolChoice{Type: "auto"}
			}
			if out.ToolChoice.Type != "none" {
				out.ToolChoice.DisableParallelToolUse = true
			}
		}
	}
	if len(out.Tools) == 0 {
		out.ToolChoice = nil
	}

	if req.User != "" {
		out.Metadata = &metadata{UserID: req.User}
	}
	if raw, ok := req.Extra["n"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n > 1 {
			return nil, invalid("n must be 1: Anthropic returns one completion")
		}
	}
	if out.OutputConfig, err = convertResponseFormat(req.Extra["response_format"]); err != nil {
		return nil, err
	}
	return out, nil
}

func userBlocks(m provider.Message) ([]block, error) {
	if isNull(m.Content) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		return textBlocks(text), nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, invalid("user content is neither a string nor an array of parts")
	}
	var out []block
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, textBlocks(p.Text)...)
		case "image_url":
			src, err := imageSourceFor(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			out = append(out, block{Type: "image", Source: src})
		default:
			return nil, invalid("unsupported content part type %q", p.Type)
		}
	}
	return out, nil
}

// textBlocks omits empty text, which Anthropic rejects.
func textBlocks(text string) []block {
	if text == "" {
		return nil
	}
	return []block{{Type: "text", Text: text}}
}

func imageSourceFor(url string) (*imageSource, error) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		media, isBase64 := strings.CutSuffix(meta, ";base64")
		if !found || !isBase64 || media == "" {
			return nil, invalid("image data URL must be data:<media type>;base64,<data>")
		}
		return &imageSource{Type: "base64", MediaType: media, Data: data}, nil
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		return &imageSource{Type: "url", URL: url}, nil
	}
	return nil, invalid("image_url must be an http(s) or data URL")
}

func assistantBlocks(m provider.Message) ([]block, error) {
	out := textBlocks(m.Text())
	if isNull(m.ToolCalls) {
		return out, nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
		return nil, invalid("tool_calls is not an array of calls")
	}
	for _, c := range calls {
		if c.Type != "" && c.Type != "function" {
			return nil, invalid("unsupported tool call type %q", c.Type)
		}
		input := json.RawMessage(`{}`)
		if args := strings.TrimSpace(c.Function.Arguments); args != "" {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
				return nil, invalid("arguments of tool call %q are not a JSON object", c.ID)
			}
			input = json.RawMessage(args)
		}
		out = append(out, block{Type: "tool_use", ID: c.ID, Name: c.Function.Name, Input: input})
	}
	return out, nil
}

func maxTokens(req *provider.ChatRequest) (int, error) {
	if req.MaxTokens != nil {
		return *req.MaxTokens, nil
	}
	if raw, ok := req.Extra["max_completion_tokens"]; ok && !isNull(raw) {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return 0, invalid("max_completion_tokens must be an integer")
		}
		return n, nil
	}
	return defaultMaxTokens, nil
}

func stopSequences(raw json.RawMessage) ([]string, error) {
	if isNull(raw) {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, invalid("stop must be a string or an array of strings")
	}
	return many, nil
}

func convertTools(raw json.RawMessage) ([]tool, error) {
	if isNull(raw) {
		return nil, nil
	}
	var in []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      bool            `json:"strict"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, invalid("tools is not an array of function tools")
	}
	out := make([]tool, 0, len(in))
	for _, t := range in {
		if t.Type != "function" {
			return nil, invalid("unsupported tool type %q", t.Type)
		}
		schema := t.Function.Parameters
		if isNull(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, tool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema, Strict: t.Function.Strict})
	}
	return out, nil
}

func convertToolChoice(raw json.RawMessage) (*toolChoice, error) {
	if isNull(raw) {
		return nil, nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "auto":
			return &toolChoice{Type: "auto"}, nil
		case "none":
			return &toolChoice{Type: "none"}, nil
		case "required":
			return &toolChoice{Type: "any"}, nil
		}
		return nil, invalid("unsupported tool_choice %q", mode)
	}
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &named) != nil || named.Type != "function" || named.Function.Name == "" {
		return nil, invalid("unsupported tool_choice")
	}
	return &toolChoice{Type: "tool", Name: named.Function.Name}, nil
}

func convertResponseFormat(raw json.RawMessage) (*outputConfig, error) {
	if isNull(raw) {
		return nil, nil
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, invalid("response_format is not an object")
	}
	switch rf.Type {
	case "text":
		return nil, nil
	case "json_schema":
		if isNull(rf.JSONSchema.Schema) {
			return nil, invalid("response_format json_schema has no schema")
		}
		format, err := json.Marshal(map[string]any{"type": "json_schema", "schema": rf.JSONSchema.Schema})
		if err != nil {
			return nil, invalid("response_format schema: %v", err)
		}
		return &outputConfig{Format: format}, nil
	}
	return nil, invalid("response_format %q has no Anthropic equivalent", rf.Type)
}

func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}
