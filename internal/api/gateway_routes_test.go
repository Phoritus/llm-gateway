package api

import (
	"net/http"
	"strings"
	"testing"
)

func newCredentialID(t *testing.T, c *client) string {
	t.Helper()
	res := createCredential(c, map[string]any{"provider": "groq", "secret": apiTestSecret})
	if res.status != http.StatusCreated {
		t.Fatalf("create credential: %d %s", res.status, res.raw)
	}
	id, _ := res.body["id"].(string)
	return id
}

func routeBody(alias string, targets ...map[string]any) map[string]any {
	list := make([]any, len(targets))
	for i, t := range targets {
		list[i] = t
	}
	return map[string]any{"alias": alias, "targets": list}
}

func target(credentialID, model string) map[string]any {
	return map[string]any{"credential_id": credentialID, "model": model}
}

func TestRoutesCRUD(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	cred := newCredentialID(t, alice)

	created := alice.do(http.MethodPost, "/api/routes",
		routeBody("my-chat", target(cred, "llama-3.3-70b"), target(cred, "llama-3.1-8b")))
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.raw)
	}
	if created.body["max_hops"] != float64(3) || created.body["free_first"] != false {
		t.Fatalf("defaults: %s", created.raw)
	}
	id, _ := created.body["id"].(string)

	list := alice.do(http.MethodGet, "/api/routes", nil)
	data, _ := list.body["data"].([]any)
	if list.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("list: %d %s", list.status, list.raw)
	}
	targets, _ := data[0].(map[string]any)["targets"].([]any)
	if len(targets) != 2 || targets[0].(map[string]any)["model"] != "llama-3.3-70b" ||
		targets[1].(map[string]any)["model"] != "llama-3.1-8b" {
		t.Fatalf("listed targets out of order: %s", list.raw)
	}

	update := routeBody("renamed", target(cred, "llama-3.1-8b"))
	update["max_hops"], update["free_first"] = 5, true
	put := alice.do(http.MethodPut, "/api/routes/"+id, update)
	if put.status != http.StatusOK || put.body["alias"] != "renamed" || put.body["max_hops"] != float64(5) {
		t.Fatalf("put: %d %s", put.status, put.raw)
	}
	list = alice.do(http.MethodGet, "/api/routes", nil)
	if !strings.Contains(list.raw, `"targets":[{"credential_id":"`+cred+`","model":"llama-3.1-8b"}]`) {
		t.Fatalf("put did not replace the chain: %s", list.raw)
	}

	reset := alice.do(http.MethodPut, "/api/routes/"+id, routeBody("renamed", target(cred, "m")))
	if reset.status != http.StatusOK || reset.body["max_hops"] != float64(3) || reset.body["free_first"] != false {
		t.Fatalf("put without max_hops or free_first must reset them: %d %s", reset.status, reset.raw)
	}

	if res := alice.do(http.MethodDelete, "/api/routes/"+id, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodDelete, "/api/routes/"+id, nil); res.status != http.StatusNotFound {
		t.Fatalf("second delete: %d %s", res.status, res.raw)
	}
	if res := alice.do(http.MethodPut, "/api/routes/not-a-uuid", update); res.status != http.StatusNotFound {
		t.Fatalf("malformed id: %d %s", res.status, res.raw)
	}
}

func TestRouteAliasIsUniquePerWorkspace(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")
	aliceCred, bobCred := newCredentialID(t, alice), newCredentialID(t, bob)

	if res := alice.do(http.MethodPost, "/api/routes", routeBody("chat", target(aliceCred, "m"))); res.status != http.StatusCreated {
		t.Fatalf("first: %d %s", res.status, res.raw)
	}
	dup := alice.do(http.MethodPost, "/api/routes", routeBody("chat", target(aliceCred, "m")))
	if dup.status != http.StatusConflict || dup.errorCode() != "alias_taken" {
		t.Fatalf("duplicate: %d %s", dup.status, dup.raw)
	}
	if res := bob.do(http.MethodPost, "/api/routes", routeBody("chat", target(bobCred, "m"))); res.status != http.StatusCreated {
		t.Fatalf("same alias in another workspace: %d %s", res.status, res.raw)
	}

	other := alice.do(http.MethodPost, "/api/routes", routeBody("other", target(aliceCred, "m")))
	id, _ := other.body["id"].(string)
	clash := alice.do(http.MethodPut, "/api/routes/"+id, routeBody("chat", target(aliceCred, "m")))
	if clash.status != http.StatusConflict || clash.errorCode() != "alias_taken" {
		t.Fatalf("rename onto a taken alias: %d %s", clash.status, clash.raw)
	}
}

func TestRouteCannotNameAnotherTenantsCredential(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	bob := env.signupAndLogin("bob@example.com")
	aliceCred, bobCred := newCredentialID(t, alice), newCredentialID(t, bob)

	stolen := bob.do(http.MethodPost, "/api/routes", routeBody("steal", target(aliceCred, "m")))
	errObj, _ := stolen.body["error"].(map[string]any)
	if stolen.status != http.StatusBadRequest || errObj["param"] != "targets" {
		t.Fatalf("create naming Alice's credential: %d %s", stolen.status, stolen.raw)
	}

	own := bob.do(http.MethodPost, "/api/routes", routeBody("mine", target(bobCred, "m")))
	id, _ := own.body["id"].(string)
	swap := bob.do(http.MethodPut, "/api/routes/"+id, routeBody("mine", target(bobCred, "m"), target(aliceCred, "m")))
	if swap.status != http.StatusBadRequest {
		t.Fatalf("update naming Alice's credential: %d %s", swap.status, swap.raw)
	}
	if list := bob.do(http.MethodGet, "/api/routes", nil); strings.Contains(list.raw, aliceCred) {
		t.Fatalf("a refused update left Alice's credential in Bob's route: %s", list.raw)
	}

	for _, res := range []response{
		alice.do(http.MethodPut, "/api/routes/"+id, routeBody("hijack", target(aliceCred, "m"))),
		alice.do(http.MethodDelete, "/api/routes/"+id, nil),
	} {
		if res.status != http.StatusNotFound {
			t.Fatalf("Alice touching Bob's route: %d %s", res.status, res.raw)
		}
	}
	if list := alice.do(http.MethodGet, "/api/routes", nil); strings.Contains(list.raw, id) {
		t.Fatalf("Alice's list shows Bob's route: %s", list.raw)
	}
}

func TestCredentialInUseCannotBeDeleted(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	cred := newCredentialID(t, alice)
	route := alice.do(http.MethodPost, "/api/routes", routeBody("chat", target(cred, "m")))
	id, _ := route.body["id"].(string)

	res := alice.do(http.MethodDelete, "/api/credentials/"+cred, nil)
	if res.status != http.StatusConflict || res.errorCode() != "credential_in_use" {
		t.Fatalf("delete in-use credential: %d %s", res.status, res.raw)
	}
	alice.do(http.MethodDelete, "/api/routes/"+id, nil)
	if res := alice.do(http.MethodDelete, "/api/credentials/"+cred, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete freed credential: %d %s", res.status, res.raw)
	}
}

func TestRouteValidation(t *testing.T) {
	env := newTestEnv(t)
	alice := env.signupAndLogin("alice@example.com")
	cred := newCredentialID(t, alice)
	good := target(cred, "m")

	withHops := func(n int) map[string]any {
		b := routeBody("chat", good)
		b["max_hops"] = n
		return b
	}
	tooMany := make([]map[string]any, 11)
	for i := range tooMany {
		tooMany[i] = target(cred, "m"+strings.Repeat("x", i))
	}

	for _, tc := range []struct {
		name, param string
		body        map[string]any
	}{
		{"empty alias", "alias", routeBody("", good)},
		{"alias with a space", "alias", routeBody("my chat", good)},
		{"alias starting with a dash", "alias", routeBody("-chat", good)},
		{"long alias", "alias", routeBody(strings.Repeat("a", 65), good)},
		{"zero hops", "max_hops", withHops(0)},
		{"too many hops", "max_hops", withHops(11)},
		{"no targets", "targets", routeBody("chat")},
		{"too many targets", "targets", routeBody("chat", tooMany...)},
		{"bad credential id", "targets", routeBody("chat", target("nope", "m"))},
		{"unknown credential", "targets", routeBody("chat", target("00000000-0000-0000-0000-000000000000", "m"))},
		{"empty model", "targets", routeBody("chat", target(cred, ""))},
		{"model with a space", "targets", routeBody("chat", target(cred, "a b"))},
		{"repeated target", "targets", routeBody("chat", good, good)},
	} {
		res := alice.do(http.MethodPost, "/api/routes", tc.body)
		errObj, _ := res.body["error"].(map[string]any)
		if res.status != http.StatusBadRequest || errObj["param"] != tc.param {
			t.Errorf("%s: %d %s", tc.name, res.status, res.raw)
		}
	}
	if list := alice.do(http.MethodGet, "/api/routes", nil); !strings.Contains(list.raw, `"data":[]`) {
		t.Fatalf("a refused create stored a route: %s", list.raw)
	}
}
