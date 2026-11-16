package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

const (
	// maxEntryBytes is the largest answer worth keeping. Past it, one entry
	// costs more of a small Redis than the provider call it saves.
	maxEntryBytes = 256 << 10
	// lookupTimeout bounds the read on the request path. A cache that is slow
	// is a miss: the request should not wait on it.
	lookupTimeout = 250 * time.Millisecond
)

// Entry is one stored answer, and the target that produced it.
type Entry struct {
	Response provider.ChatResponse `json:"response"`
	Vendor   provider.Vendor       `json:"vendor"`
	Model    string                `json:"model"`
}

// Key names the entry for a request on a route, as cache:{workspace}:{hash}.
//
// The hash covers the route and its version, so a route that has been
// rewritten stops serving what its old chain answered, with nothing to
// invalidate. Of the request it leaves out what cannot change the answer:
// model, which already resolved to the route, and stream, stream_options and
// user. The rest is decoded and encoded again, so that key order and
// whitespace, which the raw fields would carry through, cannot split one
// request into two entries.
func Key(workspaceID string, route *store.ResolvedRoute, req *provider.ChatRequest) (string, error) {
	r := *req
	r.Model, r.Stream, r.StreamOptions, r.User = "", false, nil, ""
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("cache: encode request: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Without it a number travels through float64 and two different large
	// integers can come out the same.
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return "", fmt.Errorf("cache: normalize request: %w", err)
	}
	normalized, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("cache: normalize request: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(route.ID))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(route.Version, 10)))
	h.Write([]byte{0})
	h.Write(normalized)
	return "cache:" + workspaceID + ":" + hex.EncodeToString(h.Sum(nil)), nil
}

// RequestCacheable reports whether a request's answer may be served from, and
// stored in, the cache. A request that involves tools never is: its answer is
// an instruction to act, and acting twice is not the same as acting once.
func RequestCacheable(mode store.CacheMode, req *provider.ChatRequest) bool {
	switch mode {
	case store.CacheAlways:
	case store.CacheAuto:
		if req.Temperature == nil || *req.Temperature != 0 {
			return false
		}
	default:
		return false
	}
	if present(req.Tools) || present(req.ToolChoice) ||
		present(req.Extra["functions"]) || present(req.Extra["function_call"]) {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "tool" || m.Role == "function" || present(m.ToolCalls) {
			return false
		}
	}
	return true
}

// present reports a field that was sent with a value. SDKs that serialize a
// previous assistant message send "tool_calls": null on every turn, which
// says there were none.
func present(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) > 0 && !bytes.Equal(b, []byte("null"))
}

// ResponseCacheable reports whether a complete answer may be stored: every
// choice finished and none of them calls a tool. The caller vouches that the
// stream it was assembled from ran to its end.
func ResponseCacheable(resp *provider.ChatResponse) bool {
	if len(resp.Choices) == 0 {
		return false
	}
	for _, c := range resp.Choices {
		if c.FinishReason == nil || len(c.Message.ToolCalls) > 0 {
			return false
		}
	}
	return true
}

// Replay turns a stored answer back into a stream: one chunk per choice with
// its role, content and finish reason, then the usage on a chunk of its own,
// which is where an upstream asked for usage puts it. Assembling the stream
// gives the answer back.
func Replay(resp *provider.ChatResponse) provider.Stream {
	chunk := func() *provider.ChatChunk {
		return &provider.ChatChunk{
			ID: resp.ID, Object: "chat.completion.chunk", Created: resp.Created, Model: resp.Model,
			Choices: []provider.ChunkChoice{},
		}
	}
	var chunks []*provider.ChatChunk
	for _, c := range resp.Choices {
		one := chunk()
		one.Choices = append(one.Choices, provider.ChunkChoice{
			Index:        c.Index,
			Delta:        provider.Delta{Role: c.Message.Role, Content: c.Message.Content},
			FinishReason: c.FinishReason,
		})
		chunks = append(chunks, one)
	}
	if resp.Usage != nil {
		last := chunk()
		u := *resp.Usage
		last.Usage = &u
		chunks = append(chunks, last)
	}
	return &replay{chunks: chunks}
}

type replay struct{ chunks []*provider.ChatChunk }

func (r *replay) Next() (*provider.ChatChunk, error) {
	if len(r.chunks) == 0 {
		return nil, io.EOF
	}
	next := r.chunks[0]
	r.chunks = r.chunks[1:]
	return next, nil
}

func (r *replay) Close() error { return nil }

// Exact is the exact-match cache, in Redis so every instance answers from
// the same entries and each one expires by itself.
type Exact struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewExact builds a cache whose entries live for ttl, which must be positive:
// a deployment that turned the cache off builds none.
func NewExact(rdb *redis.Client, ttl time.Duration) *Exact {
	return &Exact{rdb: rdb, ttl: ttl}
}

// Get returns the entry stored under key, or nil when there is none. An entry
// that cannot be read back is an error, which a caller treats as a miss.
func (c *Exact) Get(ctx context.Context, key string) (*Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache: get: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("cache: decode entry: %w", err)
	}
	if !ResponseCacheable(&e.Response) {
		return nil, errors.New("cache: stored entry is not a complete answer")
	}
	return &e, nil
}

// Put stores an entry and reports whether it did. An entry past maxEntryBytes
// is left out, which is not an error.
func (c *Exact) Put(ctx context.Context, key string, e Entry) (bool, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return false, fmt.Errorf("cache: encode entry: %w", err)
	}
	if len(raw) > maxEntryBytes {
		return false, nil
	}
	if err := c.rdb.Set(ctx, key, raw, c.ttl).Err(); err != nil {
		return false, fmt.Errorf("cache: set: %w", err)
	}
	return true, nil
}
