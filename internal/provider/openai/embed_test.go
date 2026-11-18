package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"llm-gateway/internal/provider"
)

func TestEmbed(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath string
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"data":[{"embedding":[0.5,-1.25,2]}],"usage":{"prompt_tokens":7}}`)
	})
	p := newProvider(srv, Endpoint{}, Options{})
	cred := provider.Credential{APIKey: testKey}

	resp, err := p.Embed(t.Context(), &provider.EmbedRequest{Model: "m", Input: "hi", Dimensions: 3}, cred)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Vector) != 3 || resp.Vector[1] != -1.25 || resp.PromptTokens != 7 {
		t.Errorf("response = %+v", resp)
	}
	if gotPath != "/v1/embeddings" || gotAuth != "Bearer "+testKey {
		t.Errorf("path %q, auth %q", gotPath, gotAuth)
	}
	if gotBody["model"] != "m" || gotBody["input"] != "hi" || gotBody["dimensions"] != float64(3) {
		t.Errorf("body = %v", gotBody)
	}

	if _, err := p.Embed(t.Context(), &provider.EmbedRequest{Model: "m", Input: "hi"}, cred); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody["dimensions"]; ok {
		t.Errorf("dimensions sent as %v, want it omitted", gotBody["dimensions"])
	}
}

func TestEmbedErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"rate limited", 429, `{"error":{"message":"slow down"}}`, provider.ErrRateLimited},
		{"bad key", 401, `{"error":{"message":"no"}}`, provider.ErrAuth},
		{"no credit", 402, `{"error":{"message":"pay"}}`, provider.ErrQuota},
		{"two items", 200, `{"data":[{"embedding":[1]},{"embedding":[2]}]}`, provider.ErrUpstream},
		{"no items", 200, `{"data":[]}`, provider.ErrUpstream},
		{"not json", 200, `<html>`, provider.ErrUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := upstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := newProvider(srv, Endpoint{}, Options{}).Embed(t.Context(), &provider.EmbedRequest{Model: "m", Input: "x"}, provider.Credential{APIKey: testKey})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEmbedHonoursContext(t *testing.T) {
	srv := upstream(t, func(w http.ResponseWriter, r *http.Request) { hang(r) })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := newProvider(srv, Endpoint{}, Options{}).Embed(ctx, &provider.EmbedRequest{Model: "m", Input: "x"}, provider.Credential{APIKey: testKey})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}
