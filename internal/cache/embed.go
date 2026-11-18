package cache

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"llm-gateway/internal/provider"
)

// Family is a set of upstream models whose vectors are comparable, so failing
// over inside one keeps a stored vector valid.
type Family struct {
	// Name is the key segment: vec:{workspace}:{Name}:{Dims}.
	Name string
	Dims int
	// Matryoshka marks a model trained to be cut: Dims is sent as
	// `dimensions`. Any other family sends none and must answer natively.
	Matryoshka bool
	// Models is the name each vendor serves the family under.
	Models map[provider.Vendor]string
}

// Families is every family the semantic cache can use.
var Families = map[string]Family{
	"gemini-embedding-2": {
		Name: "gemini-embedding-2", Dims: 1024, Matryoshka: true,
		Models: map[provider.Vendor]string{
			provider.VendorGemini:     "gemini-embedding-2",
			provider.VendorOpenRouter: "google/gemini-embedding-2",
		},
	},
	"bge-m3": {
		Name: "bge-m3", Dims: 1024,
		Models: map[provider.Vendor]string{provider.VendorOpenRouter: "baai/bge-m3"},
	},
	"mock": {
		Name: "mock", Dims: 8, Matryoshka: true,
		Models: map[provider.Vendor]string{provider.VendorMock: "mock/embed"},
	},
}

// ErrNoEmbedder means no target could be tried: none names a vendor the
// family is served by, or none has an adapter.
var ErrNoEmbedder = errors.New("no embedder available for the family")

// EmbedTarget is one credential the workspace may embed with, in the order
// the workspace prefers.
type EmbedTarget struct {
	Vendor provider.Vendor
	Cred   provider.Credential
}

// Vector is a unit-length embedding and the family it belongs to.
type Vector struct {
	Family string
	Dims   int
	Values []float32
}

// Embeddings embeds text through a workspace's credentials. Every error it
// returns is a cache miss to the caller, never a client error.
type Embeddings struct {
	adapters map[provider.Vendor]provider.Embedder
	timeout  time.Duration
}

// NewEmbeddings returns an Embeddings whose every call is bounded by timeout.
func NewEmbeddings(adapters map[provider.Vendor]provider.Embedder, timeout time.Duration) *Embeddings {
	return &Embeddings{adapters: adapters, timeout: timeout}
}

// Embed tries targets in order under one deadline for the whole call. A target
// the family does not name, or with no adapter, is skipped untried. A failed
// target, including one whose answer is unusable, hands over to the next until
// the deadline or the caller's ctx ends.
func (e *Embeddings) Embed(ctx context.Context, fam Family, targets []EmbedTarget, text string) (Vector, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	var lastErr error
	for _, t := range targets {
		model, ok := fam.Models[t.Vendor]
		adapter := e.adapters[t.Vendor]
		if !ok || adapter == nil {
			continue
		}
		req := &provider.EmbedRequest{Model: model, Input: text}
		if fam.Matryoshka {
			req.Dimensions = fam.Dims
		}
		resp, err := adapter.Embed(ctx, req, t.Cred)
		if err == nil {
			var v Vector
			if v, err = unitVector(fam, resp.Vector); err == nil {
				return v, nil
			}
		}
		lastErr = err
		if ctx.Err() != nil {
			return Vector{}, err
		}
	}
	if lastErr == nil {
		return Vector{}, ErrNoEmbedder
	}
	return Vector{}, lastErr
}

// unitVector checks an answer against the family and scales it to length 1.
// Vendors are not trusted to normalise: gemini-embedding-001 returned
// shortened vectors at length 0.61.
func unitVector(fam Family, raw []float32) (Vector, error) {
	if len(raw) != fam.Dims {
		return Vector{}, &provider.Error{Kind: provider.ErrUpstream, Detail: fmt.Sprintf("embedding has %d dimensions, family %s has %d", len(raw), fam.Name, fam.Dims)}
	}
	var sum float64
	for _, x := range raw {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return Vector{}, &provider.Error{Kind: provider.ErrUpstream, Detail: "embedding has no usable length"}
	}
	out := make([]float32, len(raw))
	for i, x := range raw {
		out[i] = float32(float64(x) / norm)
	}
	return Vector{Family: fam.Name, Dims: fam.Dims, Values: out}, nil
}
