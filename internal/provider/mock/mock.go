package mock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"
	"time"

	"llm-gateway/internal/provider"
)

// The scripted models. Any other name echoes, so an OpenAI client pointed at
// the gateway with its default model still gets an answer.
const (
	ModelEcho           = "echo"
	ModelSlow           = "slow"
	ModelRateLimited    = "rate-limited"
	ModelFail           = "fail"
	ModelContextTooLong = "context-too-long"
	ModelBreak          = "break"
)

// BreakAfter is how many content chunks ModelBreak sends before it fails.
const BreakAfter = 3

// RateLimitRetryAfter is the Retry-After ModelRateLimited reports.
const RateLimitRetryAfter = 7 * time.Second

// Provider is the scripted provider. The model name, with or without a "mock/"
// prefix, picks the script. The zero value is ready to use.
type Provider struct {
	// Delay is the pause before each chunk of ModelSlow. Zero means 200ms.
	Delay time.Duration
}

func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, _ provider.Credential) (provider.Stream, error) {
	model := strings.TrimPrefix(req.Model, "mock/")
	switch model {
	case ModelRateLimited:
		return nil, &provider.Error{Kind: provider.ErrRateLimited, RetryAfter: RateLimitRetryAfter, Detail: "mock rate limit"}
	case ModelFail:
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "mock failure"}
	case ModelContextTooLong:
		return nil, &provider.Error{Kind: provider.ErrContextTooLong, Detail: "mock context limit"}
	}

	s := &stream{
		ctx:          ctx,
		id:           "chatcmpl-mock-" + randomHex(),
		created:      time.Now().Unix(),
		model:        req.Model,
		words:        words(req),
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
		promptTokens: estimateTokens(promptChars(req)),
		breakAfter:   -1,
	}
	switch model {
	case ModelSlow:
		s.delay = p.Delay
		if s.delay == 0 {
			s.delay = 200 * time.Millisecond
		}
	case ModelBreak:
		s.breakAfter = BreakAfter
	}
	return s, nil
}

// CheckKey accepts every key: the mock has no account to check it against.
func (p *Provider) CheckKey(ctx context.Context, _ provider.Credential) error {
	return ctx.Err()
}

// stream sends one chunk per word, then a chunk with the finish reason, then
// (when asked for) a chunk with usage only, as OpenAI does.
type stream struct {
	ctx          context.Context
	id           string
	created      int64
	model        string
	words        []string
	delay        time.Duration
	breakAfter   int
	includeUsage bool
	promptTokens int

	sent       int // words sent so far
	completion int // characters sent so far
	finished   bool
	done       bool
}

func (s *stream) Next() (*provider.ChatChunk, error) {
	if s.done {
		return nil, io.EOF
	}
	if err := s.wait(); err != nil {
		s.done = true
		return nil, err
	}

	if s.breakAfter >= 0 && s.sent >= min(s.breakAfter, len(s.words)) {
		s.done = true
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "mock stream broke mid-generation"}
	}

	if s.sent < len(s.words) {
		text := s.words[s.sent]
		if s.sent > 0 {
			text = " " + text
		}
		delta := provider.Delta{Content: text}
		if s.sent == 0 {
			delta.Role = "assistant"
		}
		s.sent++
		s.completion += len(text)
		return s.chunk(provider.ChunkChoice{Delta: delta}), nil
	}

	if !s.finished {
		s.finished = true
		stop := "stop"
		if !s.includeUsage {
			s.done = true
		}
		return s.chunk(provider.ChunkChoice{FinishReason: &stop}), nil
	}

	s.done = true
	completion := estimateTokens(s.completion)
	c := s.chunk()
	c.Usage = &provider.Usage{
		PromptTokens:     s.promptTokens,
		CompletionTokens: completion,
		TotalTokens:      s.promptTokens + completion,
	}
	return c, nil
}

func (s *stream) Close() error {
	s.done = true
	return nil
}

// wait pauses for the configured delay, returning early with the context's
// error when the client goes away -- which is what stops an abandoned
// generation.
func (s *stream) wait() error {
	if s.delay == 0 {
		return s.ctx.Err()
	}
	t := time.NewTimer(s.delay)
	defer t.Stop()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *stream) chunk(choices ...provider.ChunkChoice) *provider.ChatChunk {
	if choices == nil {
		choices = []provider.ChunkChoice{}
	}
	return &provider.ChatChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: choices,
	}
}

// words is the last user message split on whitespace, or a fixed greeting
// when there is none.
func words(req *provider.ChatRequest) []string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			if w := strings.Fields(req.Messages[i].Text()); len(w) > 0 {
				return w
			}
			break
		}
	}
	return strings.Fields("Hello from the mock provider.")
}

func promptChars(req *provider.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Text())
	}
	return n
}

// estimateTokens is the spec's four characters per token, at least one.
func estimateTokens(chars int) int {
	return max(1, (chars+3)/4)
}

func randomHex() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
