package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/openai"
)

func fakeUpstream(t *testing.T, status int, events ...string) (*httptest.Server, *map[string]json.RawMessage) {
	t.Helper()
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(events[0]))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func throughOpenAI(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	return newServer(t, openai.New(openai.Endpoint{BaseURL: upstream.URL, KeyCheckPath: "/models"}, openai.Options{}), time.Minute)
}

func TestV1ForwardsUnmodelledFieldsAndAssemblesToolCalls(t *testing.T) {
	upstream, sent := fakeUpstream(t, http.StatusOK,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":1}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		`[DONE]`)
	srv := throughOpenAI(t, upstream)

	resp := post(t, srv, `{"model":"my-model","messages":[{"role":"user","content":"hi"}],"n":1,"seed":42,"response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"lookup"}}]}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	for key, want := range map[string]string{
		"model": `"echo"`, "seed": `42`, "n": `1`, "response_format": `{"type":"json_object"}`,
		"stream": `true`, "stream_options": `{"include_usage":true}`,
	} {
		if got := string((*sent)[key]); got != want {
			t.Errorf("upstream got %s = %s, want %s", key, got, want)
		}
	}

	var r provider.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	calls := r.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Arguments != `{"q":1}` {
		t.Errorf("tool calls = %+v", calls)
	}
	if r.Usage == nil || r.Usage.TotalTokens != 7 || *r.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("response = %+v", r)
	}
}

func TestV1StreamsAnOpenAICompatibleUpstream(t *testing.T) {
	upstream, _ := fakeUpstream(t, http.StatusOK,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`[DONE]`)
	resp := post(t, throughOpenAI(t, upstream), chatBody("my-model", true))
	ev := events(t, resp.Body)
	if len(ev) != 3 || ev[2] != "[DONE]" || !strings.Contains(ev[0], `"Hel"`) || !strings.Contains(ev[1], `"lo"`) {
		t.Fatalf("events = %q", ev)
	}
}

func TestV1AnswersAnUpstreamRefusalOfTheRequestAs400(t *testing.T) {
	upstream, _ := fakeUpstream(t, http.StatusBadRequest,
		`{"error":{"message":"Unrecognized request argument supplied: frobnicate","type":"invalid_request_error"}}`)
	resp := post(t, throughOpenAI(t, upstream), chatBody("my-model", false))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "upstream_invalid_request" {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "frobnicate") {
		t.Error("the upstream's own message reached the client")
	}
}
