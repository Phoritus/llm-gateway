package mock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

func request(model, text string) *provider.ChatRequest {
	content, _ := json.Marshal(text)
	return &provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: content}},
	}
}

// drain reads a stream to its end and returns the text it carried and the
// error that ended it (nil for a clean end).
func drain(t *testing.T, s provider.Stream) (string, []*provider.ChatChunk, error) {
	t.Helper()
	var b strings.Builder
	var chunks []*provider.ChatChunk
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return b.String(), chunks, nil
		}
		if err != nil {
			return b.String(), chunks, err
		}
		chunks = append(chunks, c)
		for _, ch := range c.Choices {
			b.WriteString(ch.Delta.Content)
		}
	}
}

func TestEchoStreamsTheLastUserMessageWordByWord(t *testing.T) {
	for _, model := range []string{"mock/echo", "echo", "gpt-4o"} {
		s, err := (&Provider{}).ChatStream(context.Background(), request(model, "hello there  world"), provider.Credential{})
		if err != nil {
			t.Fatalf("%s: ChatStream = %v", model, err)
		}
		text, chunks, err := drain(t, s)
		if err != nil {
			t.Fatalf("%s: stream ended with %v", model, err)
		}
		if text != "hello there world" {
			t.Errorf("%s: text = %q", model, text)
		}
		// Three words, then the finish chunk; no usage chunk unless asked.
		if len(chunks) != 4 {
			t.Fatalf("%s: %d chunks, want 4", model, len(chunks))
		}
		if chunks[0].Choices[0].Delta.Role != "assistant" {
			t.Errorf("%s: first chunk has no assistant role", model)
		}
		if fr := chunks[3].Choices[0].FinishReason; fr == nil || *fr != "stop" {
			t.Errorf("%s: last chunk finish_reason = %v, want stop", model, fr)
		}
	}
}

func TestUsageChunkOnlyWhenAsked(t *testing.T) {
	req := request("echo", "abcd efgh")
	req.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
	s, _ := (&Provider{}).ChatStream(context.Background(), req, provider.Credential{})
	_, chunks, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	last := chunks[len(chunks)-1]
	if last.Usage == nil || len(last.Choices) != 0 {
		t.Fatalf("last chunk = %+v, want a usage-only chunk", last)
	}
	if last.Usage.TotalTokens != last.Usage.PromptTokens+last.Usage.CompletionTokens || last.Usage.CompletionTokens < 1 {
		t.Errorf("usage = %+v", last.Usage)
	}
}

func TestFailuresBeforeTheStreamAreTyped(t *testing.T) {
	cases := map[string]error{
		"mock/rate-limited":     provider.ErrRateLimited,
		"mock/fail":             provider.ErrUpstream,
		"mock/context-too-long": provider.ErrContextTooLong,
	}
	for model, want := range cases {
		_, err := (&Provider{}).ChatStream(context.Background(), request(model, "hi"), provider.Credential{})
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", model, err, want)
		}
	}
	_, err := (&Provider{}).ChatStream(context.Background(), request("mock/rate-limited", "hi"), provider.Credential{})
	if got := provider.RetryAfter(err); got != RateLimitRetryAfter {
		t.Errorf("RetryAfter = %s, want %s", got, RateLimitRetryAfter)
	}
}

func TestBreakFailsMidStream(t *testing.T) {
	for _, text := range []string{"one two three four five", "short"} {
		s, err := (&Provider{}).ChatStream(context.Background(), request("mock/break", text), provider.Credential{})
		if err != nil {
			t.Fatal(err)
		}
		_, chunks, err := drain(t, s)
		if !errors.Is(err, provider.ErrUpstream) {
			t.Fatalf("%q: stream ended with %v, want ErrUpstream", text, err)
		}
		if want := min(BreakAfter, len(strings.Fields(text))); len(chunks) != want {
			t.Errorf("%q: %d chunks before the break, want %d", text, len(chunks), want)
		}
	}
}

func TestSlowStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, err := (&Provider{Delay: time.Hour}).ChatStream(ctx, request("mock/slow", "hi"), provider.Credential{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Next()
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Next = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not return after the context was cancelled")
	}
}
