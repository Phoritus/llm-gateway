package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/sse"
)

// Endpoint is one OpenAI-compatible API.
type Endpoint struct {
	BaseURL string
	// KeyCheckPath is a GET under BaseURL that needs a valid key and
	// generates nothing.
	KeyCheckPath string
	// NoStreamOptions drops stream_options from the request, for an upstream
	// that refuses the field. Such an upstream must report usage unasked.
	NoStreamOptions bool
}

var Endpoints = map[provider.Vendor]Endpoint{
	provider.VendorOpenAI:   {BaseURL: "https://api.openai.com/v1", KeyCheckPath: "/models"},
	provider.VendorGroq:     {BaseURL: "https://api.groq.com/openai/v1", KeyCheckPath: "/models"},
	provider.VendorCerebras: {BaseURL: "https://api.cerebras.ai/v1", KeyCheckPath: "/models"},
	// OpenRouter lists its models to anyone, so /models proves nothing about
	// a key; /key describes the key presented.
	provider.VendorOpenRouter: {BaseURL: "https://openrouter.ai/api/v1", KeyCheckPath: "/key"},
	// Mistral refuses unknown fields, stream_options among them, and puts
	// usage on its final chunk regardless.
	provider.VendorMistral: {BaseURL: "https://api.mistral.ai/v1", KeyCheckPath: "/models", NoStreamOptions: true},
	provider.VendorGemini:  {BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyCheckPath: "/models"},
}

// Options tunes the connection. Zero values take the defaults.
type Options struct {
	// DialTimeout bounds the TCP connect and, separately, the TLS handshake.
	DialTimeout time.Duration
}

const (
	defaultDialTimeout = 5 * time.Second

	maxErrorBody = 64 << 10
)

// Provider is one endpoint and the client every workspace's calls to it share.
type Provider struct {
	endpoint Endpoint
	client   *http.Client
}

func New(ep Endpoint, opts Options) *Provider {
	dial := orDefault(opts.DialTimeout, defaultDialTimeout)
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: dial, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: dial,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Provider{
		endpoint: ep,
		client: &http.Client{
			Transport: transport,
			// No Timeout: it would cut off a long stream.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, cred provider.Credential) (provider.Stream, error) {
	body := *req
	body.Stream = true
	if p.endpoint.NoStreamOptions {
		body.StreamOptions = nil
	}
	payload, err := json.Marshal(&body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "encode request: " + err.Error()}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "build request: " + err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, callError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		err := statusError(resp)
		resp.Body.Close()
		return nil, err
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		resp.Body.Close()
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "answered 200 with Content-Type " + resp.Header.Get("Content-Type")}
	}
	return &stream{
		ctx:    ctx,
		body:   resp.Body,
		events: sse.NewReader(resp.Body),
	}, nil
}

func (p *Provider) CheckKey(ctx context.Context, cred provider.Credential) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint.BaseURL+p.endpoint.KeyCheckPath, nil)
	if err != nil {
		return &provider.Error{Kind: provider.ErrUpstream, Detail: "build request: " + err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+cred.APIKey)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return callError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return nil
}

func callError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &provider.Error{Kind: provider.ErrTimeout, Detail: err.Error()}
	}
	return &provider.Error{Kind: provider.ErrUpstream, Detail: err.Error()}
}

type stream struct {
	ctx    context.Context
	body   io.ReadCloser
	events *sse.Reader

	sawFinishReason bool
	done            bool
	toolCallChoices map[int]bool
}

func (s *stream) Next() (*provider.ChatChunk, error) {
	if s.done {
		return nil, io.EOF
	}
	ev, err := s.events.Next()
	if err != nil {
		s.done = true
		if errors.Is(err, io.EOF) {
			if s.sawFinishReason {
				return nil, io.EOF
			}
			return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "stream ended before it finished"}
		}
		return nil, callError(s.ctx, err)
	}

	if string(ev.Data) == "[DONE]" {
		s.done = true
		return nil, io.EOF
	}
	var payload struct {
		provider.ChatChunk
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		s.done = true
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "malformed chunk: " + truncate(ev.Data)}
	}
	// Some upstreams report a failure after the stream started as an event
	// with an error object, since the status line is long gone.
	if len(payload.Error) > 0 && string(payload.Error) != "null" {
		s.done = true
		return nil, eventError(payload.Error)
	}
	chunk := payload.ChatChunk
	for i := range chunk.Choices {
		ch := &chunk.Choices[i]
		if len(ch.Delta.ToolCalls) > 0 && string(ch.Delta.ToolCalls) != "null" {
			if s.toolCallChoices == nil {
				s.toolCallChoices = map[int]bool{}
			}
			s.toolCallChoices[ch.Index] = true
		}
		if ch.FinishReason != nil {
			s.sawFinishReason = true
			// Gemini ends a tool call with "stop", and clients run tools
			// only on "tool_calls".
			if *ch.FinishReason == "stop" && s.toolCallChoices[ch.Index] {
				reason := "tool_calls"
				ch.FinishReason = &reason
			}
		}
	}
	return &chunk, nil
}

func (s *stream) Close() error {
	s.done = true
	return s.body.Close()
}

func statusError(resp *http.Response) *provider.Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	parsed := parseError(body)
	e := classify(resp.StatusCode, parsed, fmt.Sprintf("status %d: %s", resp.StatusCode, truncate(body)))
	if e.Kind == provider.ErrRateLimited {
		e.RetryAfter = retryAfter(resp.Header, time.Now())
		if e.RetryAfter == 0 {
			e.RetryAfter = parsed.retryDelay()
		}
	}
	return e
}

// code is a string at OpenAI and a number at OpenRouter, and either may be
// absent.
//
// Google adds details and wraps the object in an array.
type errorFields struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
	Details []googleDetail  `json:"details"`
}

type googleDetail struct {
	Reason     string `json:"reason"`
	RetryDelay string `json:"retryDelay"` // a protobuf Duration such as "37s"
	Violations []struct {
		QuotaID string `json:"quotaId"`
	} `json:"violations"`
}

// dailyQuotaExhausted reports a per-day quota violation, which no wait short
// of tomorrow clears.
func (f errorFields) dailyQuotaExhausted() bool {
	for _, d := range f.Details {
		for _, v := range d.Violations {
			if strings.Contains(v.QuotaID, "PerDay") {
				return true
			}
		}
	}
	return false
}

// badGoogleKey recognises Google refusing the key, which it answers with a
// 400. The OpenAI-compatible endpoint says so only in the message; the native
// API adds an API_KEY_INVALID reason.
func (f errorFields) badGoogleKey() bool {
	m := strings.ToLower(f.Message)
	if strings.Contains(m, "pass a valid api key") || strings.Contains(m, "api key not valid") {
		return true
	}
	for _, d := range f.Details {
		if d.Reason == "API_KEY_INVALID" {
			return true
		}
	}
	return false
}

func (f errorFields) retryDelay() time.Duration {
	for _, d := range f.Details {
		if delay, err := time.ParseDuration(d.RetryDelay); err == nil && delay > 0 {
			return delay
		}
	}
	return 0
}

// parseError reads OpenAI's {"error": {...}}, or the same fields at the top
// level, which is where Cerebras puts them. Google may wrap either in a
// one-element array.
func parseError(body []byte) errorFields {
	if trimmed := bytes.TrimSpace(body); bytes.HasPrefix(trimmed, []byte("[")) {
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil || len(items) == 0 {
			return errorFields{}
		}
		body = items[0]
	}
	var wrapped struct {
		Error *errorFields `json:"error"`
		errorFields
	}
	_ = json.Unmarshal(body, &wrapped)
	if wrapped.Error != nil {
		return *wrapped.Error
	}
	return wrapped.errorFields
}

// eventError classifies an error event inside a stream, where the status
// line is long gone. OpenRouter's carry the HTTP status as the code.
func eventError(raw json.RawMessage) *provider.Error {
	var fields errorFields
	_ = json.Unmarshal(raw, &fields)
	status, _ := strconv.Atoi(strings.Trim(string(fields.Code), `"`))
	return classify(status, fields, truncate(raw))
}

// classify picks the kind for an error. status is 0 when none is known.
func classify(status int, parsed errorFields, detail string) *provider.Error {
	code := strings.Trim(string(parsed.Code), `"`)
	kind := func(k error) *provider.Error { return &provider.Error{Kind: k, Detail: detail} }
	switch {
	case aboutContextLength(code, parsed.Type, parsed.Message):
		return kind(provider.ErrContextTooLong)
	case code == "insufficient_quota" || parsed.Type == "insufficient_quota":
		return kind(provider.ErrQuota)
	case parsed.badGoogleKey():
		return kind(provider.ErrAuth)
	case status == http.StatusTooManyRequests && parsed.dailyQuotaExhausted():
		return kind(provider.ErrQuota)
	}
	switch status {
	case http.StatusUnauthorized:
		return kind(provider.ErrAuth)
	case http.StatusPaymentRequired:
		return kind(provider.ErrQuota)
	case http.StatusForbidden, http.StatusNotFound:
		return kind(provider.ErrModelUnavailable)
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return kind(provider.ErrTimeout)
	case http.StatusRequestEntityTooLarge:
		return kind(provider.ErrContextTooLong)
	case http.StatusTooManyRequests:
		return kind(provider.ErrRateLimited)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return kind(provider.ErrInvalidRequest)
	}
	return kind(provider.ErrUpstream)
}

// aboutContextLength recognises the ways these upstreams say a request is too
// long for the model: OpenAI's code, and the wording of those without one.
func aboutContextLength(code, typ, message string) bool {
	if code == "context_length_exceeded" || typ == "context_length_exceeded" {
		return true
	}
	m := strings.ToLower(message)
	for _, phrase := range []string{
		"context length", "context_length", "context window", "maximum context",
		"reduce the length of the messages", "exceeds the maximum number of tokens",
	} {
		if strings.Contains(m, phrase) {
			return true
		}
	}
	return false
}

// retryAfter reads how long the upstream asked to be left alone: OpenAI's
// millisecond header when present, else Retry-After in seconds or as a date.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Retry-After-Ms")), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func truncate(b []byte) string {
	const limit = 300
	if len(b) > limit {
		return strings.ToValidUTF8(string(b[:limit]), "") + "..."
	}
	return strings.ToValidUTF8(string(b), "?")
}
