package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/provider/mock"
	"llm-gateway/internal/store"
)

func listModels(t *testing.T, srv *httptest.Server, authorization string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestModelsListsTheWorkspacesAliasesInOpenAIsShape(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := listModels(t, srv, "Bearer "+testKey)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s, want 200", resp.StatusCode, raw)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"object": "list",
		"data": []any{
			map[string]any{"id": "fast", "object": "model", "created": float64(testAliasCreated.Unix()), "owned_by": "llm-gateway"},
			map[string]any{"id": renamedAlias, "object": "model", "created": float64(testAliasCreated.Unix()), "owned_by": "llm-gateway"},
		},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("body = %s\nwant   %s", gotJSON, wantJSON)
	}
	if strings.Contains(string(raw), mock.ModelEcho) {
		t.Fatalf("the list names an upstream model: %s", raw)
	}
}

func TestModelsListIsEmptyNotNullForAWorkspaceWithoutRoutes(t *testing.T) {
	srv, _ := limitedServer(t, &mock.Provider{}, func(d *Deps) {
		d.Keys = keyFor(store.ActiveGatewayKey{ID: "k3", WorkspaceID: "ws3", OwnerVerified: true})
	})
	resp := listModels(t, srv, "Bearer "+testKey)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != `{"object":"list","data":[]}` {
		t.Fatalf("%d %s, want 200 with an empty data array", resp.StatusCode, raw)
	}
}

func TestModelsRefusesWhatChatRefuses(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	cases := map[string]struct {
		authorization string
		status        int
		code          string
	}{
		"no header":        {"", http.StatusUnauthorized, "invalid_api_key"},
		"unknown key":      {"Bearer gwk_nope", http.StatusUnauthorized, "invalid_api_key"},
		"unverified owner": {"Bearer " + unverifiedKey, http.StatusForbidden, "email_not_verified"},
	}
	for name, tc := range cases {
		resp := listModels(t, srv, tc.authorization)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != tc.status || errorCode(t, body) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, resp.StatusCode, body, tc.status, tc.code)
		}
	}
}

func TestModelsCountsFailedAuthenticationsAgainstTheAddress(t *testing.T) {
	keys := &countingKeys{}
	srv, _ := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Keys = keys })
	for range int(authFailPerIP.Burst) {
		drain(listModels(t, srv, "Bearer gwk_nope"))
	}
	if resp := listModels(t, srv, "Bearer gwk_nope"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 past the failed-authentication limit", resp.StatusCode)
	}
}

func TestModelsSpendsARequestFromTheKey(t *testing.T) {
	rule := tight(2)
	srv, limiter := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Limits.KeyRequests = rule })

	for i := range 2 {
		resp := listModels(t, srv, "Bearer "+testKey)
		drain(resp)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-RateLimit-Remaining-Requests") == "" {
			t.Fatalf("request %d: status %d, remaining %q", i, resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining-Requests"))
		}
	}
	resp := listModels(t, srv, "Bearer "+testKey)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, want 429 with Retry-After once the key's requests are spent", resp.StatusCode)
	}
	if got := level(t, limiter, scopeKeyTokens, "k1", roomy); got != roomy.Burst {
		t.Fatalf("the key's token bucket holds %v, want it untouched", got)
	}
}

type failingAliases struct{}

func (failingAliases) RouteAliases(context.Context, string) ([]store.RouteAlias, error) {
	return nil, errors.New("postgres is down")
}

func TestModelsHidesAStoreFailure(t *testing.T) {
	srv, _ := limitedServer(t, &mock.Provider{}, func(d *Deps) { d.Aliases = failingAliases{} })
	resp := listModels(t, srv, "Bearer "+testKey)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(string(body), "postgres") {
		t.Fatalf("%d %s, want a 500 that does not repeat the cause", resp.StatusCode, body)
	}
}

type keyFor store.ActiveGatewayKey

func (k keyFor) Authenticate(context.Context, string) (store.ActiveGatewayKey, error) {
	return store.ActiveGatewayKey(k), nil
}
