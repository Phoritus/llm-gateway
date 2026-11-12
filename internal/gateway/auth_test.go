package gateway

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/provider/mock"
)

func TestRequestsWithoutAValidKeyAreRefused(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	cases := map[string]struct {
		authorization string
		status        int
		code          string
	}{
		"no header":        {"", http.StatusUnauthorized, "invalid_api_key"},
		"basic scheme":     {"Basic " + testKey, http.StatusUnauthorized, "invalid_api_key"},
		"empty bearer":     {"Bearer ", http.StatusUnauthorized, "invalid_api_key"},
		"unknown key":      {"Bearer gwk_nope", http.StatusUnauthorized, "invalid_api_key"},
		"unverified owner": {"Bearer " + unverifiedKey, http.StatusForbidden, "email_not_verified"},
	}
	for name, tc := range cases {
		resp := postAs(t, srv, tc.authorization, chatBody("mock/echo", false))
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != tc.status || errorCode(t, body) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, resp.StatusCode, body, tc.status, tc.code)
		}
		if tc.status == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: a 401 without WWW-Authenticate", name)
		}
	}

	if resp := postAs(t, srv, "bearer "+testKey, chatBody("mock/echo", false)); resp.StatusCode != http.StatusOK {
		t.Errorf("a lowercase scheme: status %d, want 200", resp.StatusCode)
	}
}

func TestTheKeyIsCheckedBeforeTheBody(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := postAs(t, srv, "", `{`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAnUnknownModelIsNotFound(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody(missingAlias, false))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || errorCode(t, body) != "model_not_found" {
		t.Fatalf("%d %s, want 404 model_not_found", resp.StatusCode, body)
	}
}

func TestAVendorWithoutAnAdapterIsABadGateway(t *testing.T) {
	srv := newServer(t, &mock.Provider{}, time.Minute)
	resp := post(t, srv, chatBody(adapterlessPrefix+"gpt-4o", false))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "provider_unavailable" {
		t.Fatalf("%d %s, want 502 provider_unavailable", resp.StatusCode, body)
	}
}

type recording struct {
	mock.Provider
	model string
	cred  provider.Credential
}

func (p *recording) ChatStream(ctx context.Context, req *provider.ChatRequest, cred provider.Credential) (provider.Stream, error) {
	p.model, p.cred = req.Model, cred
	return p.Provider.ChatStream(ctx, req, cred)
}

func TestTheTargetsModelAndKeyReachTheAdapter(t *testing.T) {
	p := &recording{}
	srv := newServer(t, p, time.Minute)
	if resp := post(t, srv, chatBody(renamedAlias, false)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if p.model != mock.ModelEcho || p.cred.APIKey != testSecret {
		t.Errorf("adapter got model %q key %q, want the target's model and opened secret", p.model, p.cred.APIKey)
	}
}
