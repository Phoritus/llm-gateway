package anthropic

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

// Options tunes the connection. Zero values take the defaults.
type Options struct {
	// BaseURL is the API origin; tests point it at a fake.
	BaseURL string
	// DialTimeout bounds the TCP connect and, separately, the TLS handshake.
	DialTimeout time.Duration
}

const (
	defaultBaseURL     = "https://api.anthropic.com"
	apiVersion         = "2026-10-21"
	defaultDialTimeout = 5 * time.Second

	maxErrorBody = 64 << 10
)

// Provider is the Anthropic API and the client every workspace's calls to it
// share.
type Provider struct {
	baseURL string
	client  *http.Client
}

func New(opts Options) *Provider {
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
	base := opts.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return &Provider{
		baseURL: strings.TrimRight(base, "/"),
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

func (p *Provider) newRequest(ctx context.Context, method, path string, body io.Reader, cred provider.Credential) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "build request: " + err.Error()}
	}
	r.Header.Set("x-api-key", cred.APIKey)
	r.Header.Set("anthropic-version", apiVersion)
	r.Header.Set("content-type", "application/json")
	return r, nil
}

func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest, cred provider.Credential) (provider.Stream, error) {
	wire, err := translate(req)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "encode request: " + err.Error()}
	}

	httpReq, err := p.newRequest(ctx, http.MethodPost, "/v1/messages", bytes.NewReader(payload), cred)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("accept", "text/event-stream")

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
		ctx:          ctx,
		body:         resp.Body,
		events:       sse.NewReader(resp.Body),
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
		toolIndex:    map[int]int{},
	}, nil
}

func (p *Provider) CheckKey(ctx context.Context, cred provider.Credential) error {
	httpReq, err := p.newRequest(ctx, http.MethodGet, "/v1/models", nil, cred)
	if err != nil {
		return err
	}
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

func statusError(resp *http.Response) *provider.Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	message := parsed.Error.Message
	if message == "" {
		message = truncate(body)
	}
	e := classify(resp.StatusCode, parsed.Error.Type, message)
	if e.Kind == provider.ErrRateLimited {
		e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return e
}

// classify picks the kind for an error. status is 0 for an error event inside
// a stream, where the status line is long gone and only the type is left.
func classify(status int, errType, message string) *provider.Error {
	detail := truncate([]byte(fmt.Sprintf("status %d %s: %s", status, errType, message)))
	kind := func(k error) *provider.Error { return &provider.Error{Kind: k, Detail: detail} }
	lower := strings.ToLower(message)
	switch {
	case errType == "request_too_large", status == http.StatusRequestEntityTooLarge,
		strings.Contains(lower, "prompt is too long"), strings.Contains(lower, "context limit"),
		strings.Contains(lower, "context window"):
		return kind(provider.ErrContextTooLong)
	case errType == "billing_error", status == http.StatusPaymentRequired,
		strings.Contains(lower, "credit balance"):
		return kind(provider.ErrQuota)
	case status == http.StatusUnauthorized, errType == "authentication_error":
		return kind(provider.ErrAuth)
	case status == http.StatusForbidden, errType == "permission_error",
		status == http.StatusNotFound, errType == "not_found_error":
		return kind(provider.ErrModelUnavailable)
	case status == http.StatusTooManyRequests, errType == "rate_limit_error":
		return kind(provider.ErrRateLimited)
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout, errType == "timeout_error":
		return kind(provider.ErrTimeout)
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity, errType == "invalid_request_error":
		return kind(provider.ErrInvalidRequest)
	}
	return kind(provider.ErrUpstream)
}

// retryAfter reads Retry-After as seconds or as a date.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
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
