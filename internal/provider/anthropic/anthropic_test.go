package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

const testKey = "sk-ant-test-0123456789abcdef"

func upstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newProvider(srv *httptest.Server, opts Options) *Provider {
	opts.BaseURL = srv.URL
	return New(opts)
}

// hang holds a handler until the client goes away. net/http notices that
// only once the request body has been read, so the body is read first.
func hang(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

func sseHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func event(w http.ResponseWriter, name, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	w.(http.Flusher).Flush()
}

func drain(t *testing.T, s provider.Stream) ([]*provider.ChatChunk, error) {
	t.Helper()
	var out []*provider.ChatChunk
	for {
		c, err := s.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, c)
	}
}

func start(t *testing.T, p *Provider, body string) (provider.Stream, error) {
	t.Helper()
	return p.ChatStream(context.Background(), chatRequest(t, body), provider.Credential{APIKey: testKey})
}

const simpleBody = `{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`

const messageStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}}}`

func textAnswer(w http.ResponseWriter) {
	sseHeader(w)
	event(w, "message_start", messageStart)
	event(w, "ping", `{"type":"ping"}`)
	event(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	event(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`)
	event(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`)
	event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	event(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`)
	event(w, "message_stop", `{"type":"message_stop"}`)
}

func TestSendsTheMessagesRequest(t *testing.T) {
	var got map[string]json.RawMessage
	var header http.Header
	var method, path string
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		header, method, path = r.Header, r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		textAnswer(w)
	})
	s, err := start(t, newProvider(srv, Options{}), `{"model":"claude-x","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := drain(t, s); err != nil {
		t.Fatal(err)
	}

	if method != http.MethodPost || path != "/v1/messages" {
		t.Errorf("request: %s %s", method, path)
	}
	for k, want := range map[string]string{
		"x-api-key": testKey, "anthropic-version": "2026-10-21", "content-type": "application/json", "accept": "text/event-stream",
	} {
		if header.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, header.Get(k), want)
		}
	}
	for k, want := range map[string]string{"model": `"claude-x"`, "stream": `true`, "max_tokens": `32000`} {
		if string(got[k]) != want {
			t.Errorf("body %s = %s, want %s", k, got[k], want)
		}
	}
	if _, ok := got["system"]; !ok {
		t.Error("system was not sent")
	}
}

func TestInvalidRequestNeverReachesTheUpstream(t *testing.T) {
	called := false
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := start(t, newProvider(srv, Options{}), `{"model":"c","messages":[{"role":"user","content":"x"}],"n":3}`)
	if !errors.Is(err, provider.ErrInvalidRequest) || called {
		t.Fatalf("err = %v, upstream called = %v", err, called)
	}
}

func TestStreamsATextAnswer(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) { textAnswer(w) })
	s, err := start(t, newProvider(srv, Options{}), simpleBody)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want role, two texts, finish: %+v", len(chunks), chunks)
	}
	for _, c := range chunks {
		if c.ID != "msg_1" || c.Model != "claude-x" || c.Object != "chat.completion.chunk" || c.Created == 0 || len(c.Choices) != 1 {
			t.Errorf("chunk = %+v", c)
		}
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" || chunks[1].Choices[0].Delta.Content != "Hel" || chunks[2].Choices[0].Delta.Content != "lo" {
		t.Errorf("deltas = %+v", chunks)
	}
	if f := chunks[3].Choices[0].FinishReason; f == nil || *f != "stop" {
		t.Errorf("finish = %v", f)
	}
}

func TestUsageChunkOnlyWhenAsked(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) { textAnswer(w) })
	p := newProvider(srv, Options{})

	s, err := start(t, p, simpleBody)
	if err != nil {
		t.Fatal(err)
	}
	chunks, _ := drain(t, s)
	s.Close()
	for _, c := range chunks {
		if c.Usage != nil {
			t.Errorf("usage sent unasked: %+v", c.Usage)
		}
	}

	s, err = start(t, p, `{"model":"c","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err = drain(t, s)
	if err != nil {
		t.Fatal(err)
	}
	last := chunks[len(chunks)-1]
	if last.Choices == nil || len(last.Choices) != 0 || last.Usage == nil {
		t.Fatalf("last chunk = %+v", last)
	}
	if *last.Usage != (provider.Usage{PromptTokens: 15, CompletionTokens: 4, TotalTokens: 19}) {
		t.Errorf("usage = %+v", *last.Usage)
	}
}

func TestStreamsToolCallsAndAssemblesThem(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		event(w, "message_start", messageStart)
		event(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		event(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`)
		event(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
		event(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		event(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"weather","input":{}}}`)
		event(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`)
		event(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
		event(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Oslo\"}"}}`)
		event(w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
		event(w, "content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"time","input":{}}}`)
		event(w, "content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)
		event(w, "content_block_stop", `{"type":"content_block_stop","index":2}`)
		event(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`)
		event(w, "message_stop", `{"type":"message_stop"}`)
	})
	s, err := start(t, newProvider(srv, Options{}), simpleBody)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}

	var deltas []string
	for _, c := range chunks {
		if tc := c.Choices[0].Delta.ToolCalls; len(tc) > 0 {
			deltas = append(deltas, string(tc))
		}
		if c.Choices[0].Delta.Content != "" {
			t.Errorf("thinking leaked as content: %q", c.Choices[0].Delta.Content)
		}
	}
	want := []string{
		`[{"index":0,"id":"toolu_1","type":"function","function":{"name":"weather","arguments":""}}]`,
		`[{"index":0,"function":{"arguments":"{\"city\":"}}]`,
		`[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]`,
		`[{"index":1,"id":"toolu_2","type":"function","function":{"name":"time","arguments":""}}]`,
		`[{"index":1,"function":{"arguments":"{}"}}]`,
	}
	if strings.Join(deltas, "\n") != strings.Join(want, "\n") {
		t.Errorf("tool deltas =\n%s\nwant\n%s", strings.Join(deltas, "\n"), strings.Join(want, "\n"))
	}

	var a provider.Assembler
	for _, c := range chunks {
		a.Add(c)
	}
	r := a.Response()
	calls := r.Choices[0].Message.ToolCalls
	if len(calls) != 2 ||
		calls[0].ID != "toolu_1" || calls[0].Function.Name != "weather" || calls[0].Function.Arguments != `{"city":"Oslo"}` ||
		calls[1].ID != "toolu_2" || calls[1].Function.Name != "time" || calls[1].Function.Arguments != `{}` {
		t.Errorf("calls = %+v", calls)
	}
	if f := r.Choices[0].FinishReason; f == nil || *f != "tool_calls" {
		t.Errorf("finish = %v", f)
	}
}

func TestFinishReasons(t *testing.T) {
	for stop, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "pause_turn": "stop",
		"max_tokens": "length", "model_context_window_exceeded": "length",
		"tool_use": "tool_calls", "refusal": "content_filter", "something_new": "stop",
	} {
		t.Run(stop, func(t *testing.T) {
			srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
				sseHeader(w)
				event(w, "message_start", messageStart)
				event(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":1}}`)
				event(w, "message_stop", `{"type":"message_stop"}`)
			})
			s, err := start(t, newProvider(srv, Options{}), simpleBody)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			chunks, err := drain(t, s)
			if err != nil {
				t.Fatal(err)
			}
			if f := chunks[len(chunks)-1].Choices[0].FinishReason; f == nil || *f != want {
				t.Errorf("finish = %v, want %s", f, want)
			}
		})
	}
}

func TestClassifiesUpstreamRefusals(t *testing.T) {
	body := func(typ, msg string) string {
		return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
	}
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		kind   error
		retry  time.Duration
	}{
		{"bad key", 401, nil, body("authentication_error", "invalid x-api-key"), provider.ErrAuth, 0},
		{"no credit", 400, nil, body("invalid_request_error", "Your credit balance is too low to access the Anthropic API."), provider.ErrQuota, 0},
		{"billing", 402, nil, body("billing_error", "payment needed"), provider.ErrQuota, 0},
		{"forbidden", 403, nil, body("permission_error", "no access"), provider.ErrModelUnavailable, 0},
		{"unknown model", 404, nil, body("not_found_error", "model: nope"), provider.ErrModelUnavailable, 0},
		{"rate limit seconds", 429, http.Header{"Retry-After": {"12"}}, body("rate_limit_error", "slow down"), provider.ErrRateLimited, 12 * time.Second},
		{"rate limit no header", 429, nil, body("rate_limit_error", "slow down"), provider.ErrRateLimited, 0},
		{"context by wording", 400, nil, body("invalid_request_error", "prompt is too long: 250000 tokens > 200000 maximum"), provider.ErrContextTooLong, 0},
		{"context limit", 400, nil, body("invalid_request_error", "input length and `max_tokens` exceed context limit"), provider.ErrContextTooLong, 0},
		{"too large", 413, nil, body("request_too_large", "Request exceeds the maximum allowed number of bytes."), provider.ErrContextTooLong, 0},
		{"timeout 504", 504, nil, `upstream timed out`, provider.ErrTimeout, 0},
		{"timeout type", 500, nil, body("timeout_error", "took too long"), provider.ErrTimeout, 0},
		{"bad request", 400, nil, body("invalid_request_error", "messages: roles must alternate"), provider.ErrInvalidRequest, 0},
		{"unprocessable", 422, nil, `{}`, provider.ErrInvalidRequest, 0},
		{"overloaded", 529, nil, body("overloaded_error", "Overloaded"), provider.ErrUpstream, 0},
		{"api error", 500, nil, body("api_error", "internal"), provider.ErrUpstream, 0},
		{"html 502", 502, nil, `<html>bad gateway</html>`, provider.ErrUpstream, 0},
		{"redirect", 302, http.Header{"Location": {"https://example.com/"}}, ``, provider.ErrUpstream, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header()[k] = v
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			_, err := start(t, newProvider(srv, Options{}), simpleBody)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("err = %v, want %v", err, tc.kind)
			}
			if got := provider.RetryAfter(err); got != tc.retry {
				t.Errorf("RetryAfter = %v, want %v", got, tc.retry)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Error("the error quotes the key")
			}
		})
	}
}

func TestRetryAfterAsDate(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if got := retryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); got != 90*time.Second {
		t.Errorf("date form = %v", got)
	}
	if got := retryAfter("junk", now); got != 0 {
		t.Errorf("junk = %v", got)
	}
}

func TestAnErrorEventMidStreamIsTyped(t *testing.T) {
	for typ, kind := range map[string]error{
		"overloaded_error": provider.ErrUpstream,
		"rate_limit_error": provider.ErrRateLimited,
	} {
		t.Run(typ, func(t *testing.T) {
			srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
				sseHeader(w)
				event(w, "message_start", messageStart)
				event(w, "error", `{"type":"error","error":{"type":"`+typ+`","message":"try later"}}`)
			})
			s, err := start(t, newProvider(srv, Options{}), simpleBody)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.Next(); err != nil {
				t.Fatalf("first chunk: %v", err)
			}
			if _, err := s.Next(); !errors.Is(err, kind) {
				t.Fatalf("err = %v, want %v", err, kind)
			}
			if _, err := s.Next(); !errors.Is(err, kind) {
				t.Fatalf("Next after the failure = %v, want it to stay failed", err)
			}
		})
	}
}

func TestAStreamThatStopsEarlyIsAFailure(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		event(w, "message_start", messageStart)
		event(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`)
	})
	s, err := start(t, newProvider(srv, Options{}), simpleBody)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if !errors.Is(err, provider.ErrUpstream) || len(chunks) != 2 {
		t.Fatalf("chunks %d, err %v", len(chunks), err)
	}
}

func TestNon200WithoutEventStreamIsUpstreamFailure(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	if _, err := start(t, newProvider(srv, Options{}), simpleBody); !errors.Is(err, provider.ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnEndedContextStopsTheWaitForTheFirstEvent(t *testing.T) {
	const wait = 100 * time.Millisecond

	t.Run("no headers", func(t *testing.T) {
		srv := upstream(t, func(w http.ResponseWriter, r *http.Request) { hang(r) })
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		_, err := newProvider(srv, Options{}).ChatStream(ctx, chatRequest(t, simpleBody), provider.Credential{APIKey: testKey})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the context's", err)
		}
	})

	t.Run("headers but no event", func(t *testing.T) {
		srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
			sseHeader(w)
			w.Write([]byte(": processing\n\n"))
			w.(http.Flusher).Flush()
			hang(r)
		})
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		s, err := newProvider(srv, Options{}).ChatStream(ctx, chatRequest(t, simpleBody), provider.Credential{APIKey: testKey})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Next(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the context's", err)
		}
	})
}

func TestMessageStartIsTheFirstChunk(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		event(w, "message_start", messageStart)
		hang(r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := newProvider(srv, Options{}).ChatStream(ctx, chatRequest(t, simpleBody), provider.Credential{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunk, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk = %+v, want the assistant's role", chunk)
	}
}

func TestNextReturnsPromptlyWhenTheCallerGoesAway(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		event(w, "message_start", messageStart)
		hang(r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := newProvider(srv, Options{}).ChatStream(ctx, chatRequest(t, simpleBody), provider.Credential{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = s.Next()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Next took %v after cancellation", time.Since(start))
	}
}

func TestCloseIsSafeAtAnyTime(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) { textAnswer(w) })
	s, err := start(t, newProvider(srv, Options{}), simpleBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next after Close = %v", err)
	}
}

func TestCheckKey(t *testing.T) {
	var path, key, version string
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		path, key, version = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		if key != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return
		}
		w.Write([]byte(`{"data":[]}`))
	})
	p := newProvider(srv, Options{})
	if err := p.CheckKey(context.Background(), provider.Credential{APIKey: testKey}); err != nil {
		t.Fatalf("valid key: %v", err)
	}
	if path != "/v1/models" || version != "2026-10-21" {
		t.Errorf("path %q, version %q", path, version)
	}
	if err := p.CheckKey(context.Background(), provider.Credential{APIKey: "sk-wrong"}); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("wrong key: %v, want ErrAuth", err)
	}
}
