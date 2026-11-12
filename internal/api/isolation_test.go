// Cross-tenant isolation, kept apart because it is the one test whose failure
// means a tenant can read another tenant's data, not that a flow is broken.

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Cross-tenant isolation, at both layers: the store answers membership
// questions per workspace, and a session cannot be made to act in a workspace
// its user does not belong to.
//
// Membership is the whole gate now that there are no roles, so this is the
// test that matters most: if the join in IdentityByTokenHash were ever
// dropped, a session row naming another tenant's workspace would be enough to
// read that tenant's data.
func TestCrossTenantIsolation(t *testing.T) {
	env := newTestEnv(t)

	alice := env.signupAndLogin("alice@example.com")
	env.signupAndLogin("bob@example.com")

	aliceWorkspace := env.workspaceOf("alice@example.com")
	bobWorkspace := env.workspaceOf("bob@example.com")
	aliceID := env.userID("alice@example.com")
	bobID := env.userID("bob@example.com")

	if aliceWorkspace == bobWorkspace {
		t.Fatal("the two signups share a workspace")
	}

	ctx := context.Background()

	// Store layer: each workspace knows only its own member.
	for _, tc := range []struct {
		workspace, user, who string
		want                 bool
	}{
		{aliceWorkspace, aliceID, "Alice in her own workspace", true},
		{bobWorkspace, bobID, "Bob in his own workspace", true},
		{aliceWorkspace, bobID, "Bob in Alice's workspace", false},
		{bobWorkspace, aliceID, "Alice in Bob's workspace", false},
	} {
		got, err := env.store.IsMember(ctx, tc.workspace, tc.user)
		if err != nil {
			t.Fatalf("IsMember(%s): %v", tc.who, err)
		}
		if got != tc.want {
			t.Fatalf("IsMember(%s) = %v, want %v", tc.who, got, tc.want)
		}
	}

	// API layer: Alice's own session reports her own workspace, which it took
	// from the session row, so there was no parameter to tamper with.
	me := alice.do(http.MethodGet, "/api/me", nil)
	if me.status != http.StatusOK {
		t.Fatalf("Alice's /api/me: status = %d, body = %s", me.status, me.raw)
	}
	workspace, _ := me.body["workspace"].(map[string]any)
	if id, _ := workspace["id"].(string); id != aliceWorkspace {
		t.Fatalf("Alice's workspace = %q, want her own %q", id, aliceWorkspace)
	}
	if strings.Contains(me.raw, "bob@example.com") || strings.Contains(me.raw, bobWorkspace) {
		t.Fatalf("Alice's /api/me mentions Bob's tenant:\n%s", me.raw)
	}

	// A session minted for Alice against Bob's workspace -- what a tampered
	// workspace switcher would produce -- must not authenticate at all. The
	// membership join is what refuses it.
	intruder := env.sessionFor("alice@example.com", bobWorkspace)
	if res := intruder.do(http.MethodGet, "/api/me", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("Alice's session in Bob's workspace: status = %d, body = %s, want 401",
			res.status, res.raw)
	}
}

func TestCredentialsAreIsolatedBetweenTenants(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")

	created := createCredential(alice, map[string]any{"provider": "groq", "secret": apiTestSecret})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	id, _ := created.body["id"].(string)

	list := bob.do(http.MethodGet, "/api/credentials", nil)
	if list.status != http.StatusOK || strings.Contains(list.raw, id) {
		t.Fatalf("Bob's list shows Alice's credential: %d %s", list.status, list.raw)
	}
	if res := bob.do(http.MethodDelete, "/api/credentials/"+id, nil); res.status != http.StatusNotFound {
		t.Fatalf("Bob's delete: %d %s", res.status, res.raw)
	}
	mine := alice.do(http.MethodGet, "/api/credentials", nil)
	if !strings.Contains(mine.raw, id) {
		t.Fatalf("Alice lost her credential: %s", mine.raw)
	}
}
