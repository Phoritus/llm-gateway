package openai

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

const testKey = "sk-test-0123456789abcdef"

func upstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newProvider(srv *httptest.Server, ep Endpoint, opts Options) *Provider {
	ep.BaseURL = srv.URL + "/v1"
	if ep.KeyCheckPath == "" {
		ep.KeyCheckPath = "/models"
	}
	return New(ep, opts)
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

func event(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}

func chatRequest(t *testing.T, body string) *provider.ChatRequest {
	t.Helper()
	var req provider.ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return &req
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

func TestStreamsAnOpenAICompatibleUpstream(t *testing.T) {
	var got map[string]json.RawMessage
	var auth, accept, path string
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		auth, accept, path = r.Header.Get("Authorization"), r.Header.Get("Accept"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		sseHeader(w)
		w.Write([]byte(": keep-alive\n\n"))
		event(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`)
		event(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		event(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
		event(w, `[DONE]`)
	})
	p := newProvider(srv, Endpoint{}, Options{})
	req := chatRequest(t, `{"model":"llama","messages":[{"role":"user","content":"hi"}],"seed":7,"stream_options":{"include_usage":true}}`)

	s, err := p.ChatStream(context.Background(), req, provider.Credential{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if err != nil {
		t.Fatal(err)
	}

	if path != "/v1/chat/completions" || auth != "Bearer "+testKey || accept != "text/event-stream" {
		t.Errorf("request: path %q, auth %q, accept %q", path, auth, accept)
	}
	for key, want := range map[string]string{
		"model": `"llama"`, "stream": `true`, "seed": `7`, "stream_options": `{"include_usage":true}`,
	} {
		if string(got[key]) != want {
			t.Errorf("upstream body %s = %s, want %s", key, got[key], want)
		}
	}
	if req.Stream {
		t.Error("ChatStream modified the caller's request")
	}
	if len(chunks) != 3 || chunks[0].Choices[0].Delta.Content != "Hi" || chunks[2].Usage == nil || chunks[2].Usage.TotalTokens != 4 {
		t.Errorf("chunks = %+v", chunks)
	}
}

func TestNoStreamOptionsDropsTheField(t *testing.T) {
	var got map[string]json.RawMessage
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		sseHeader(w)
		event(w, `[DONE]`)
	})
	p := newProvider(srv, Endpoint{NoStreamOptions: true}, Options{})
	req := chatRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`)
	s, err := p.ChatStream(context.Background(), req, provider.Credential{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, ok := got["stream_options"]; ok {
		t.Errorf("stream_options was sent: %v", got)
	}
}

func geminiRateLimit(quotaID string) string {
	return `[{"error":{"code":429,"message":"You exceeded your current quota.","status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"` + quotaID + `"}]},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37s"}]}}]`
}

func TestClassifiesUpstreamRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		kind   error
		retry  time.Duration
	}{
		{"bad key", 401, nil, `{"error":{"message":"Invalid API key","code":"invalid_api_key"}}`, provider.ErrAuth, 0},
		{"no credit", 402, nil, `{"error":{"message":"Insufficient credits","code":402}}`, provider.ErrQuota, 0},
		{"model not allowed", 403, nil, `{"error":{"message":"no access to model"}}`, provider.ErrModelUnavailable, 0},
		{"unknown model", 404, nil, `{"error":{"message":"The model does not exist","code":"model_not_found"}}`, provider.ErrModelUnavailable, 0},
		{"per-minute limit", 429, http.Header{"Retry-After": {"12"}}, `{"error":{"message":"Rate limit reached"}}`, provider.ErrRateLimited, 12 * time.Second},
		{"limit in ms", 429, http.Header{"Retry-After-Ms": {"1500"}, "Retry-After": {"9"}}, `{}`, provider.ErrRateLimited, 1500 * time.Millisecond},
		{"quota as 429", 429, http.Header{"Retry-After": {"3"}}, `{"error":{"message":"exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, provider.ErrQuota, 0},
		{"context by code", 400, nil, `{"error":{"message":"too long","code":"context_length_exceeded"}}`, provider.ErrContextTooLong, 0},
		{"context by wording", 400, nil, `{"error":{"message":"Please reduce the length of the messages or completion."}}`, provider.ErrContextTooLong, 0},
		{"context, unwrapped", 400, nil, `{"message":"Please reduce the length of the messages","type":"invalid_request_error","code":"context_length_exceeded"}`, provider.ErrContextTooLong, 0},
		{"context as 413", 413, nil, `{}`, provider.ErrContextTooLong, 0},
		{"bad parameter", 400, nil, `{"error":{"message":"Unrecognized request argument: foo"}}`, provider.ErrInvalidRequest, 0},
		{"validation", 422, nil, `{"detail":[{"type":"extra_forbidden"}]}`, provider.ErrInvalidRequest, 0},
		{"gemini per-minute", 429, nil, geminiRateLimit("GenerateRequestsPerMinutePerProjectPerModel-FreeTier"), provider.ErrRateLimited, 37 * time.Second},
		{"gemini per-day", 429, nil, geminiRateLimit("GenerateRequestsPerDayPerProjectPerModel-FreeTier"), provider.ErrQuota, 0},
		{"gemini retry header wins", 429, http.Header{"Retry-After": {"5"}}, geminiRateLimit("PerMinute"), provider.ErrRateLimited, 5 * time.Second},
		{"gemini bad key", 400, nil, `[{"error":{"code":400,"message":"Please pass a valid API key","status":"INVALID_ARGUMENT"}}]`, provider.ErrAuth, 0},
		{"gemini native bad key", 400, nil, `[{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID"}]}}]`, provider.ErrAuth, 0},
		{"gemini context", 400, nil, `[{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}]`, provider.ErrContextTooLong, 0},
		{"gemini bad argument", 400, nil, `[{"error":{"code":400,"message":"Invalid JSON payload received.","status":"INVALID_ARGUMENT"}}]`, provider.ErrInvalidRequest, 0},
		{"gateway timeout", 504, nil, `upstream timed out`, provider.ErrTimeout, 0},
		{"server error", 500, nil, `<html>oops</html>`, provider.ErrUpstream, 0},
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
			p := newProvider(srv, Endpoint{}, Options{})
			_, err := p.ChatStream(context.Background(), chatRequest(t, `{"model":"m","messages":[]}`), provider.Credential{APIKey: testKey})
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

func TestAnErrorEventMidStreamIsTyped(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		event(w, `{"id":"c","choices":[{"index":0,"delta":{"content":"partial"}}]}`)
		event(w, `{"id":"c","error":{"code":429,"message":"Rate limit exceeded upstream"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`)
	})
	s, err := newProvider(srv, Endpoint{}, Options{}).ChatStream(context.Background(),
		chatRequest(t, `{"model":"m","messages":[]}`), provider.Credential{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	chunks, err := drain(t, s)
	if len(chunks) != 1 || !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("got %d chunks and %v, want 1 chunk then ErrRateLimited", len(chunks), err)
	}
}

func TestAStreamThatStopsEarlyIsAFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		events []string
		broken bool
	}{
		"cut before finishing":  {[]string{`{"choices":[{"index":0,"delta":{"content":"a"}}]}`}, true},
		"finished without DONE": {[]string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`}, false},
		"malformed chunk":       {[]string{`{"choices":[`}, true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
				sseHeader(w)
				for _, e := range tc.events {
					event(w, e)
				}
			})
			s, err := newProvider(srv, Endpoint{}, Options{}).ChatStream(context.Background(),
				chatRequest(t, `{"model":"m","messages":[]}`), provider.Credential{APIKey: testKey})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, err = drain(t, s)
			if tc.broken != errors.Is(err, provider.ErrUpstream) {
				t.Fatalf("err = %v, broken = %v", err, tc.broken)
			}
		})
	}
}

func TestFirstByteTimeout(t *testing.T) {
	opts := Options{FirstByteTimeout: 100 * time.Millisecond}

	t.Run("no headers", func(t *testing.T) {
		srv := upstream(t, func(w http.ResponseWriter, r *http.Request) { hang(r) })
		_, err := newProvider(srv, Endpoint{}, opts).ChatStream(context.Background(),
			chatRequest(t, `{"model":"m","messages":[]}`), provider.Credential{APIKey: testKey})
		if !errors.Is(err, provider.ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	})

	t.Run("headers but no event", func(t *testing.T) {
		srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
			sseHeader(w)
			w.Write([]byte(": processing\n\n"))
			w.(http.Flusher).Flush()
			hang(r)
		})
		s, err := newProvider(srv, Endpoint{}, opts).ChatStream(context.Background(),
			chatRequest(t, `{"model":"m","messages":[]}`), provider.Credential{APIKey: testKey})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Next(); !errors.Is(err, provider.ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
	})

	t.Run("a slow stream after the first event is not cut off", func(t *testing.T) {
		srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
			sseHeader(w)
			event(w, `{"choices":[{"index":0,"delta":{"content":"a"}}]}`)
			time.Sleep(3 * opts.FirstByteTimeout)
			event(w, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
			event(w, `[DONE]`)
		})
		s, err := newProvider(srv, Endpoint{}, opts).ChatStream(context.Background(),
			chatRequest(t, `{"model":"m","messages":[]}`), provider.Credential{APIKey: testKey})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if chunks, err := drain(t, s); err != nil || len(chunks) != 2 {
			t.Fatalf("got %d chunks, err %v", len(chunks), err)
		}
	})
}

func TestNextReturnsPromptlyWhenTheCallerGoesAway(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		event(w, `{"choices":[{"index":0,"delta":{"content":"a"}}]}`)
		hang(r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := newProvider(srv, Endpoint{}, Options{}).ChatStream(ctx,
		chatRequest(t, `{"model":"m","messages":[]}`), provider.Credential{APIKey: testKey})
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

func TestCheckKey(t *testing.T) {
	var path, auth string
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if auth != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[]}`))
	})
	p := newProvider(srv, Endpoint{KeyCheckPath: "/key"}, Options{})
	if err := p.CheckKey(context.Background(), provider.Credential{APIKey: testKey}); err != nil {
		t.Fatalf("valid key: %v", err)
	}
	if path != "/v1/key" {
		t.Errorf("path = %q, want /v1/key", path)
	}
	if err := p.CheckKey(context.Background(), provider.Credential{APIKey: "sk-wrong"}); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("wrong key: %v, want ErrAuth", err)
	}
}

func TestCheckKeyTakesGoogleBadKeyAsAuth(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":400,"message":"Please pass a valid API key","status":"INVALID_ARGUMENT"}}`))
	})
	p := newProvider(srv, Endpoint{}, Options{})
	if err := p.CheckKey(context.Background(), provider.Credential{APIKey: testKey}); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
}

func TestEndpointsCoverOnlyOpenAICompatibleVendors(t *testing.T) {
	for v, ep := range Endpoints {
		if !strings.HasPrefix(ep.BaseURL, "https://") || ep.KeyCheckPath == "" {
			t.Errorf("%s: %+v", v, ep)
		}
	}
	for _, v := range []provider.Vendor{provider.VendorAnthropic, provider.VendorMock} {
		if _, ok := Endpoints[v]; ok {
			t.Errorf("%s is served by its own adapter", v)
		}
	}
}
