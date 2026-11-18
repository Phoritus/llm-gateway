package mock

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

func embed(t *testing.T, p *Provider, model, text string, dims int) ([]float32, error) {
	t.Helper()
	resp, err := p.Embed(t.Context(), &provider.EmbedRequest{Model: model, Input: text, Dimensions: dims}, provider.Credential{})
	if err != nil {
		return nil, err
	}
	return resp.Vector, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func TestEmbedIsDeterministicAndWordBased(t *testing.T) {
	p := &Provider{}
	a, err := embed(t, p, "mock/embed", "How do I reset my password?", 32)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := embed(t, p, "embed", "how do i RESET my password", 32)
	if cosine(a, again) < 0.9999 {
		t.Errorf("same words, different case: cosine %v", cosine(a, again))
	}
	related, _ := embed(t, p, "embed", "reset password steps", 32)
	unrelated, _ := embed(t, p, "embed", "tomato soup with basil", 32)
	if cosine(a, related) <= cosine(a, unrelated) {
		t.Errorf("shared words %v, unrelated %v", cosine(a, related), cosine(a, unrelated))
	}
}

func TestEmbedDimensionsAndTokens(t *testing.T) {
	p := &Provider{}
	if v, _ := embed(t, p, "embed", "x", 0); len(v) != 8 {
		t.Errorf("default size = %d, want 8", len(v))
	}
	if v, _ := embed(t, p, "embed", "x", 20); len(v) != 20 {
		t.Errorf("size = %d, want 20", len(v))
	}
	resp, _ := p.Embed(t.Context(), &provider.EmbedRequest{Model: "embed", Input: strings.Repeat("a", 9)}, provider.Credential{})
	if resp.PromptTokens != 3 {
		t.Errorf("prompt tokens = %d, want 3", resp.PromptTokens)
	}
}

func TestEmbedScripts(t *testing.T) {
	p := &Provider{Delay: 10 * time.Millisecond}
	if _, err := embed(t, p, "mock/fail", "x", 0); !errors.Is(err, provider.ErrUpstream) {
		t.Errorf("fail: %v", err)
	}
	if _, err := embed(t, p, "rate-limited", "x", 0); !errors.Is(err, provider.ErrRateLimited) {
		t.Errorf("rate-limited: %v", err)
	}
	start := time.Now()
	if _, err := embed(t, p, "slow", "x", 0); err != nil || time.Since(start) < 10*time.Millisecond {
		t.Errorf("slow: err %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	slow := &Provider{Delay: time.Minute}
	if _, err := slow.Embed(ctx, &provider.EmbedRequest{Model: "slow", Input: "x"}, provider.Credential{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("slow with deadline: %v", err)
	}
}
