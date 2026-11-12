package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/telemetry"
)

func newServer(t *testing.T, p provider.Provider, timeout time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/v1", Routes(Deps{Provider: p, Timeout: timeout})))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func chatBody(model string, stream bool) string {
	b, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []map[string]string{{"role": "user", "content": "one two three four"}},
		// A field the gateway does not model must not be refused.
		"presence_penalty": 0.5,
	})
	return string(b)
}

// events reads every SSE data payload until the body ends.
func events(t *testing.T, body io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}

func errorCode(t *testing.T, payload []byte) string {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(payload, &e); err != nil {
		t.Fatalf("not an error envelope: %s", payload)
	}
	return e.Error.Code
}

func TestStreamSendsChunksThenDone(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody("mock/echo", true))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	ev := events(t, resp.Body)
	if len(ev) < 2 || ev[len(ev)-1] != "[DONE]" {
		t.Fatalf("events = %v, want chunks then [DONE]", ev)
	}
	var text strings.Builder
	for _, e := range ev[:len(ev)-1] {
		var c provider.ChatChunk
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			t.Fatalf("chunk %q: %v", e, err)
		}
		if c.Object != "chat.completion.chunk" {
			t.Errorf("object = %q", c.Object)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if text.String() != "one two three four" {
		t.Errorf("streamed text = %q", text.String())
	}
}

func TestNonStreamAssemblesOneCompletion(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody("mock/echo", false))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var r provider.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.Object != "chat.completion" || len(r.Choices) != 1 || r.Choices[0].Message.Content != "one two three four" {
		t.Errorf("response = %+v", r)
	}
	if r.Usage == nil || r.Usage.TotalTokens == 0 {
		t.Errorf("usage = %+v, want it reported", r.Usage)
	}
}

func TestUpstreamFailuresBeforeTheFirstByteAreStatuses(t *testing.T) {
	cases := []struct {
		model  string
		status int
		code   string
	}{
		{"mock/rate-limited", http.StatusTooManyRequests, "upstream_rate_limited"},
		{"mock/context-too-long", http.StatusRequestEntityTooLarge, "context_length_exceeded"},
		{"mock/fail", http.StatusBadGateway, "upstream_error"},
	}
	srv := newServer(t, &mock.Provider{}, time.Minute)
	for _, stream := range []bool{true, false} {
		for _, tc := range cases {
			resp := post(t, srv, chatBody(tc.model, stream))
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status {
				t.Errorf("%s stream=%v: status = %d, want %d", tc.model, stream, resp.StatusCode, tc.status)
			}
			if got := errorCode(t, body); got != tc.code {
				t.Errorf("%s: code = %q, want %q", tc.model, got, tc.code)
			}
		}
	}
	resp := post(t, srv, chatBody("mock/rate-limited", true))
	if got := resp.Header.Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want 7", got)
	}
}

func TestMidStreamFailureEndsWithAnErrorEvent(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody("mock/break", true))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; the failure came after the first byte", resp.StatusCode)
	}
	ev := events(t, resp.Body)
	if len(ev) != mock.BreakAfter+1 {
		t.Fatalf("events = %v, want %d chunks and an error", ev, mock.BreakAfter)
	}
	if got := errorCode(t, []byte(ev[len(ev)-1])); got != "upstream_error" {
		t.Errorf("last event code = %q", got)
	}
}

func TestMidStreamFailureWithoutStreamingIsAStatus(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody("mock/break", false))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestDeadlineIsAnsweredAsATimeout(t *testing.T) {
	srv := newServer(t, &mock.Provider{Delay: time.Hour}, 50*time.Millisecond)
	resp := post(t, srv, chatBody("mock/slow", true))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "upstream_timeout" {
		t.Fatalf("status = %d body = %s, want 502 upstream_timeout", resp.StatusCode, body)
	}
}

// watched reports when a generation starts and when its context ends.
type watched struct {
	mock.Provider
	started, cancelled chan struct{}
}

func (w *watched) ChatStream(ctx context.Context, req *provider.ChatRequest, c provider.Credential) (provider.Stream, error) {
	close(w.started)
	go func() { <-ctx.Done(); close(w.cancelled) }()
	return w.Provider.ChatStream(ctx, req, c)
}

// The generation would take an hour and the deadline is an hour, so the only
// thing that can end its context within the test is the client hanging up.
func TestClientDisconnectStopsTheUpstream(t *testing.T) {
	p := &watched{Provider: mock.Provider{Delay: time.Hour}, started: make(chan struct{}), cancelled: make(chan struct{})}
	srv := newServer(t, p, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(chatBody("mock/slow", true)))
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the generation never started")
	}
	cancel()

	select {
	case <-p.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream context was not cancelled after the client disconnected")
	}
}

func TestInvalidRequestsAreRefused(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"not json":       {`{`, http.StatusBadRequest, "invalid_json"},
		"no model":       {`{"messages":[{"role":"user","content":"x"}]}`, http.StatusBadRequest, "invalid_value"},
		"no messages":    {`{"model":"echo","messages":[]}`, http.StatusBadRequest, "invalid_value"},
		"no role":        {`{"model":"echo","messages":[{"content":"x"}]}`, http.StatusBadRequest, "invalid_value"},
		"body too large": {`{"model":"echo","messages":[{"role":"user","content":"` + strings.Repeat("a", maxBodyBytes) + `"}]}`, http.StatusRequestEntityTooLarge, "body_too_large"},
	}
	for name, tc := range cases {
		resp := post(t, srv, tc.body)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != tc.status || errorCode(t, body) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, resp.StatusCode, body, tc.status, tc.code)
		}
	}
}

func TestUnknownPathIsAnOpenAIError(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp, err := http.Get(srv.URL + "/v1/models/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || errorCode(t, body) != "not_found" {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

// productionChain wraps Routes in the same middleware cmd/gateway does, so a
// socket deadline set through http.ResponseController is proven to reach the
// connection through every wrapper.
func productionChain(t *testing.T, p provider.Provider, timeout time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v1/", http.StripPrefix("/v1", Routes(Deps{Provider: p, Timeout: timeout})))
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := httptest.NewServer(otelhttp.NewHandler(
		telemetry.RequestID(telemetry.AccessLog(logger)(telemetry.Route(mux))), "http.server"))
	t.Cleanup(srv.Close)
	return srv
}

// closeWatched reports when the handler releases its stream, which it does on
// the way out.
type closeWatched struct {
	mock.Provider
	closed chan struct{}
}

func (c *closeWatched) ChatStream(ctx context.Context, req *provider.ChatRequest, cred provider.Credential) (provider.Stream, error) {
	s, err := c.Provider.ChatStream(ctx, req, cred)
	if err != nil {
		return nil, err
	}
	return closeSignal{Stream: s, closed: c.closed}, nil
}

type closeSignal struct {
	provider.Stream
	closed chan struct{}
}

func (c closeSignal) Close() error {
	close(c.closed)
	return c.Stream.Close()
}

func TestDeadlineCoversASlowBody(t *testing.T) {
	srv := productionChain(t, &mock.Provider{}, 300*time.Millisecond)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Promise 1000 bytes, send one, then nothing.
	fmt.Fprint(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n{")

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response to a stalled body: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestTimeout || errorCode(t, body) != "request_timeout" {
		t.Fatalf("%d %s, want 408 request_timeout", resp.StatusCode, body)
	}
}

func TestDeadlineReleasesAClientThatStopsReading(t *testing.T) {
	p := &closeWatched{closed: make(chan struct{})}
	srv := productionChain(t, p, 300*time.Millisecond)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// An echo of a million words is far more than the socket buffers hold, so
	// the handler blocks writing once the client stops reading.
	body := `{"model":"echo","stream":true,"messages":[{"role":"user","content":"` +
		strings.Repeat("a ", 1<<20) + `"}]}`
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)

	select {
	case <-p.closed:
	case <-time.After(writeGrace + 10*time.Second):
		t.Fatal("the handler was still writing long after the deadline and its grace period")
	}
}

func TestMalformedMessagesAreRefused(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	for name, body := range map[string]string{
		"blank model":    `{"model":"   ","messages":[{"role":"user","content":"x"}]}`,
		"unknown role":   `{"model":"echo","messages":[{"role":"wizard","content":"x"}]}`,
		"number content": `{"model":"echo","messages":[{"role":"user","content":42}]}`,
		"object content": `{"model":"echo","messages":[{"role":"user","content":{"a":1}}]}`,
	} {
		resp := post(t, srv, body)
		got, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || errorCode(t, got) != "invalid_value" {
			t.Errorf("%s: %d %s, want 400 invalid_value", name, resp.StatusCode, got)
		}
	}
	for name, body := range map[string]string{
		"null content":  `{"model":"echo","messages":[{"role":"assistant","content":null,"tool_calls":[]}]}`,
		"content parts": `{"model":"echo","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`,
		"developer":     `{"model":"echo","messages":[{"role":"developer","content":"x"}]}`,
	} {
		if resp := post(t, srv, body); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200", name, resp.StatusCode)
		}
	}
}
