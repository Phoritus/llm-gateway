package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"llm-gateway/internal/cache"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
	"llm-gateway/internal/telemetry"
)

// ResponseCache stores whole answers by key. *cache.Exact is the production
// one. Get returns nil for a key it does not hold; Put reports whether it
// kept the entry.
type ResponseCache interface {
	Get(ctx context.Context, key string) (*cache.Entry, error)
	Put(ctx context.Context, key string, e cache.Entry) (bool, error)
}

// SemanticCache stores answers by the meaning of the last question asked in a
// context. *cache.Semantic is the production one. Lookup returns nil when no
// stored question is close enough.
type SemanticCache interface {
	Lookup(ctx context.Context, workspaceID string, fam cache.Family, ctxHash cache.ContextHash, v cache.Vector) (*cache.Entry, error)
	Put(ctx context.Context, workspaceID string, fam cache.Family, ctxHash cache.ContextHash, v cache.Vector, e cache.Entry) error
}

// vendorCache is the vendor label of a request the cache answered. A hit's
// time to first token is close to nothing, and under the vendor that produced
// the stored answer it would pull that vendor's histogram down.
const vendorCache = "cache"

// The results a request can have at the cache, which are the label on its
// counter.
const (
	cacheHit         = "hit"
	cacheSemanticHit = "semantic_hit"
	cacheMiss        = "miss"
	cacheStored      = "stored"
	cacheSkipped     = "skipped"
	cacheError       = "error"
)

var cacheResults, _ = meter.Int64Counter("gateway.cache",
	metric.WithDescription("What the cache did with a request: hit, semantic_hit, miss, stored, skipped (not cacheable) or error."))

func countCache(result string) {
	cacheResults.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
}

// cacheLookup is what the cache step found for one request.
type cacheLookup struct {
	// key is empty when the request is not cacheable, or the cache is off.
	key string
	// hit is the stored answer, when there is one.
	hit      *cache.Entry
	semantic *semanticQuery
}

type semanticQuery struct {
	fam     cache.Family
	ctxHash cache.ContextHash
	vector  cache.Vector
}

// lookupCache is step 4 of the pipeline. It never fails the request: a cache
// that errors, or holds something unreadable, is a miss.
func (d Deps) lookupCache(r *http.Request, workspaceID string, route *store.ResolvedRoute, req *provider.ChatRequest) cacheLookup {
	if d.Cache == nil {
		return cacheLookup{}
	}
	if !cache.RequestCacheable(route, req) {
		countCache(cacheSkipped)
		return cacheLookup{}
	}
	ctx := r.Context()
	key, err := cache.Key(workspaceID, route, req)
	if err != nil {
		d.logCache(ctx, "cache key failed", err)
		return cacheLookup{}
	}
	hit, err := d.Cache.Get(ctx, key)
	var query *semanticQuery
	switch {
	case err != nil:
		d.logCache(ctx, "cache lookup failed, treated as a miss", err)
	case hit != nil:
		countCache(cacheHit)
	default:
		if hit, query = d.lookupSemantic(ctx, workspaceID, route, req); hit != nil {
			countCache(cacheSemanticHit)
		} else {
			countCache(cacheMiss)
		}
	}
	return cacheLookup{key: key, hit: hit, semantic: query}
}

func (d Deps) lookupSemantic(ctx context.Context, workspaceID string, route *store.ResolvedRoute, req *provider.ChatRequest) (*cache.Entry, *semanticQuery) {
	if d.Semantic == nil || d.Embeddings == nil || !route.Settings.SemanticCache {
		return nil, nil
	}
	n := len(req.Messages)
	if n == 0 || req.Messages[n-1].Role != "user" {
		return nil, nil
	}
	text := req.Messages[n-1].Text()
	if text == "" {
		return nil, nil
	}
	fam, targets, ok := d.Embeddings.Plan(route.Targets)
	if !ok {
		return nil, nil
	}
	ctxHash, err := cache.HashContext(route, req)
	if err != nil {
		d.logCache(ctx, "semantic context hash failed", err)
		return nil, nil
	}
	vector, err := d.Embeddings.Embed(ctx, fam, targets, text)
	if err != nil {
		d.logCache(ctx, "embedding failed, treated as a miss", err)
		return nil, nil
	}
	query := &semanticQuery{fam: fam, ctxHash: ctxHash, vector: vector}
	hit, err := d.Semantic.Lookup(ctx, workspaceID, fam, ctxHash, vector)
	if err != nil {
		d.logCache(ctx, "semantic lookup failed, treated as a miss", err)
	}
	return hit, query
}

// storeAnswer keeps a complete answer under the key its request missed on. It
// runs detached, like the rate-limit reconcile: the response has been sent and
// nobody waits on the write.
func (d Deps) storeAnswer(ctx context.Context, workspaceID string, cached cacheLookup, t store.ResolvedTarget, resp *provider.ChatResponse) {
	key := cached.key
	if key == "" {
		return
	}
	if !cache.ResponseCacheable(resp) {
		countCache(cacheSkipped)
		return
	}
	entry := cache.Entry{Response: *resp, Vendor: t.Vendor, Model: t.Model}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer cancel()
		stored, err := d.Cache.Put(ctx, key, entry)
		switch {
		case err != nil:
			d.logCache(ctx, "cache write failed", err)
		case stored:
			countCache(cacheStored)
		default:
			countCache(cacheSkipped)
		}
		if q := cached.semantic; q != nil {
			if err := d.Semantic.Put(ctx, workspaceID, q.fam, q.ctxHash, q.vector, entry); err != nil {
				d.logCache(ctx, "semantic cache write failed", err)
			}
		}
	}()
}

func (d Deps) logCache(ctx context.Context, msg string, err error) {
	countCache(cacheError)
	d.logger().WarnContext(ctx, msg,
		slog.String("request_id", telemetry.RequestIDFrom(ctx)),
		slog.String("error", err.Error()))
}

// assembled passes a stream through and keeps the answer its chunks add up
// to, so the chunks can go to a client while the whole is put together for a
// non-streaming response or for the cache.
type assembled struct {
	provider.Stream
	answer provider.Assembler
	// complete is set once the stream ended on its own, with io.EOF. Only
	// then is the answer the whole answer.
	complete bool
}

func (s *assembled) Next() (*provider.ChatChunk, error) {
	chunk, err := s.Stream.Next()
	switch {
	case err == nil:
		s.answer.Add(chunk)
		if s.answer.TooLarge() {
			return nil, &provider.Error{Kind: provider.ErrUpstream, Detail: "answer longer than provider.MaxAnswerBytes"}
		}
	case errors.Is(err, io.EOF):
		s.complete = true
	}
	return chunk, err
}
