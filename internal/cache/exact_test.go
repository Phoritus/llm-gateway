package cache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/store"
)

func request(t *testing.T, body string) *provider.ChatRequest {
	t.Helper()
	var req provider.ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("request %s: %v", body, err)
	}
	return &req
}

var testRoute = &store.ResolvedRoute{ID: "route-1", Version: 7}

func keyOf(t *testing.T, workspace string, route *store.ResolvedRoute, body string) string {
	t.Helper()
	key, err := Key(workspace, route, request(t, body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	return key
}

const baseRequest = `{"model":"chat","temperature":0,"seed":7,
	"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
	"stop":["a","b"],"response_format":{"type":"text","strict":true}}`

func TestKeyIgnoresWhatCannotChangeTheAnswer(t *testing.T) {
	want := keyOf(t, "ws", testRoute, baseRequest)
	if !regexp.MustCompile(`^cache:ws:[0-9a-f]{64}$`).MatchString(want) {
		t.Fatalf("key = %q, want cache:{workspace}:{sha256}", want)
	}
	for name, body := range map[string]string{
		"key order and whitespace": `{"response_format":{"strict":true,  "type":"text"},"stop":[ "a", "b" ],
			"messages":[{"content":[{"text":"hi","type":"text"}],"role":"user"}],"seed":7,"temperature":0,"model":"chat"}`,
		"another alias for the route": strings.Replace(baseRequest, `"model":"chat"`, `"model":"renamed"`, 1),
		"streaming":                   strings.Replace(baseRequest, `"seed":7`, `"seed":7,"stream":true,"stream_options":{"include_usage":true}`, 1),
		"a user label":                strings.Replace(baseRequest, `"seed":7`, `"seed":7,"user":"u-1"`, 1),
		"temperature written as 0.0":  strings.Replace(baseRequest, `"temperature":0`, `"temperature":0.0`, 1),
	} {
		if got := keyOf(t, "ws", testRoute, body); got != want {
			t.Errorf("%s: key differs", name)
		}
	}
}

func TestKeySeparatesWhatCanChangeTheAnswer(t *testing.T) {
	base := keyOf(t, "ws", testRoute, baseRequest)
	for name, body := range map[string]string{
		"another message":     strings.Replace(baseRequest, `"text":"hi"`, `"text":"hi "`, 1),
		"another temperature": strings.Replace(baseRequest, `"temperature":0`, `"temperature":0.5`, 1),
		"another seed":        strings.Replace(baseRequest, `"seed":7`, `"seed":8`, 1),
		"a seed past float64": strings.Replace(baseRequest, `"seed":7`, `"seed":9007199254740993`, 1),
		"stop in another order": strings.Replace(baseRequest, `"stop":["a","b"]`,
			`"stop":["b","a"]`, 1),
		"a token cap": strings.Replace(baseRequest, `"seed":7`, `"seed":7,"max_tokens":5`, 1),
	} {
		if keyOf(t, "ws", testRoute, body) == base {
			t.Errorf("%s: key is the same", name)
		}
	}
	if keyOf(t, "ws", testRoute, strings.Replace(baseRequest, `"seed":7`, `"seed":9007199254740993`, 1)) ==
		keyOf(t, "ws", testRoute, strings.Replace(baseRequest, `"seed":7`, `"seed":9007199254740992`, 1)) {
		t.Error("two integers that round to one float64 share a key")
	}

	if keyOf(t, "other", testRoute, baseRequest) == base {
		t.Error("two workspaces share a key")
	}
	if keyOf(t, "ws", &store.ResolvedRoute{ID: "route-2", Version: 7}, baseRequest) == base {
		t.Error("two routes share a key")
	}
	if keyOf(t, "ws", &store.ResolvedRoute{ID: "route-1", Version: 8}, baseRequest) == base {
		t.Error("a rewritten route kept its key")
	}
}

func TestKeyLeavesTheRequestAlone(t *testing.T) {
	req := request(t, strings.Replace(baseRequest, `"seed":7`, `"seed":7,"stream":true,"user":"u-1"`, 1))
	if _, err := Key("ws", testRoute, req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "chat" || !req.Stream || req.User != "u-1" {
		t.Fatalf("Key changed the request it was given: %+v", req)
	}
}

func TestRequestCacheable(t *testing.T) {
	const plain = `"messages":[{"role":"user","content":"hi"}]`
	for _, tc := range []struct {
		name string
		mode store.CacheMode
		body string
		want bool
	}{
		{"auto at temperature 0", store.CacheAuto, `{` + plain + `,"temperature":0}`, true},
		{"auto at temperature 0.0", store.CacheAuto, `{` + plain + `,"temperature":0.0}`, true},
		{"auto with no temperature", store.CacheAuto, `{` + plain + `}`, false},
		{"auto at temperature 0.2", store.CacheAuto, `{` + plain + `,"temperature":0.2}`, false},
		{"always at any temperature", store.CacheAlways, `{` + plain + `,"temperature":1}`, true},
		{"always with no temperature", store.CacheAlways, `{` + plain + `}`, true},
		{"off at temperature 0", store.CacheOff, `{` + plain + `,"temperature":0}`, false},
		{"an unknown mode", store.CacheMode(""), `{` + plain + `,"temperature":0}`, false},
		{"tools", store.CacheAlways, `{` + plain + `,"tools":[{"type":"function"}]}`, false},
		{"an empty tools list", store.CacheAlways, `{` + plain + `,"tools":[]}`, false},
		{"tool_choice", store.CacheAlways, `{` + plain + `,"tool_choice":"auto"}`, false},
		{"functions", store.CacheAlways, `{` + plain + `,"functions":[{"name":"f"}]}`, false},
		{"function_call", store.CacheAlways, `{` + plain + `,"function_call":"auto"}`, false},
		{"null tool fields", store.CacheAlways,
			`{` + plain + `,"tools":null,"tool_choice":null,"functions":null,"function_call":null}`, true},
		{"a tool message", store.CacheAlways,
			`{"messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"c1","content":"4"}]}`, false},
		{"a function message", store.CacheAlways,
			`{"messages":[{"role":"function","name":"f","content":"4"}]}`, false},
		{"an assistant message that called a tool", store.CacheAlways,
			`{"messages":[{"role":"assistant","tool_calls":[{"id":"c1"}]}]}`, false},
		{"an assistant message with null tool_calls", store.CacheAlways,
			`{"messages":[{"role":"assistant","content":"hello","tool_calls":null},{"role":"user","content":"hi"}]}`, true},
	} {
		if got := RequestCacheable(&store.ResolvedRoute{Cache: tc.mode}, request(t, tc.body)); got != tc.want {
			t.Errorf("%s: cacheable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAWorkspaceWithCachingOffCachesNothing(t *testing.T) {
	route := &store.ResolvedRoute{Cache: store.CacheAlways, Settings: store.WorkspaceSettings{CacheOff: true}}
	if RequestCacheable(route, request(t, `{"messages":[{"role":"user","content":"hi"}],"temperature":0}`)) {
		t.Fatal("a route set to always was cached in a workspace that turned caching off")
	}
}

func finished(reason string) *string { return &reason }

func answer(choices ...provider.ResponseItem) *provider.ChatResponse {
	return &provider.ChatResponse{
		ID: "chatcmpl-1", Object: "chat.completion", Created: 1700000000, Model: "llama",
		Choices: choices,
		Usage:   &provider.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}
}

func choice(index int, content string, reason *string) provider.ResponseItem {
	return provider.ResponseItem{
		Index: index, Message: provider.ResponseMessage{Role: "assistant", Content: content}, FinishReason: reason,
	}
}

func TestResponseCacheable(t *testing.T) {
	toolCall := choice(0, "", finished("tool_calls"))
	toolCall.Message.ToolCalls = []provider.ToolCall{{ID: "c1", Type: "function"}}

	for _, tc := range []struct {
		name string
		resp *provider.ChatResponse
		want bool
	}{
		{"one finished choice", answer(choice(0, "hello", finished("stop"))), true},
		{"a choice cut off at its token cap", answer(choice(0, "hel", finished("length"))), true},
		{"every choice finished", answer(choice(0, "a", finished("stop")), choice(1, "b", finished("stop"))), true},
		{"no choices", answer(), false},
		{"a choice with no finish reason", answer(choice(0, "a", finished("stop")), choice(1, "b", nil)), false},
		{"a tool call", answer(toolCall), false},
	} {
		if got := ResponseCacheable(tc.resp); got != tc.want {
			t.Errorf("%s: cacheable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func assemble(t *testing.T, s provider.Stream) *provider.ChatResponse {
	t.Helper()
	var a provider.Assembler
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			return a.Response()
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q", c.Object)
		}
		a.Add(c)
	}
}

func TestReplayAssemblesBackToTheAnswer(t *testing.T) {
	for name, resp := range map[string]*provider.ChatResponse{
		"one choice":    answer(choice(0, "hello there", finished("stop"))),
		"two choices":   answer(choice(0, "first", finished("stop")), choice(1, "second", finished("length"))),
		"empty content": answer(choice(0, "", finished("stop"))),
	} {
		if got := assemble(t, Replay(resp)); !reflect.DeepEqual(got, resp) {
			t.Errorf("%s: replay assembled to %+v, want %+v", name, got, resp)
		}
	}

	noUsage := answer(choice(0, "hello", finished("stop")))
	noUsage.Usage = nil
	if got := assemble(t, Replay(noUsage)); !reflect.DeepEqual(got, noUsage) {
		t.Errorf("without usage: replay assembled to %+v, want %+v", got, noUsage)
	}
}

func TestReplayPutsUsageOnAChunkOfItsOwn(t *testing.T) {
	s := Replay(answer(choice(0, "hello", finished("stop"))))
	first, _ := s.Next()
	last, _ := s.Next()
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("third Next = %v, want io.EOF", err)
	}
	if first.Usage != nil || len(first.Choices) != 1 {
		t.Errorf("first chunk = %+v, want the choice and no usage", first)
	}
	// A client that did not ask for usage is sent no chunk without choices,
	// which the gateway tells by exactly this shape.
	if last.Usage == nil || last.Choices == nil || len(last.Choices) != 0 {
		t.Errorf("last chunk = %+v, want usage and an empty choices list", last)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func testExact(t *testing.T) (*Exact, *redis.Client, string) {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL to run the exact cache's Redis tests")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_TEST_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	// This test's own key, deleted by name: REDIS_TEST_URL is shared with
	// every other package's tests.
	key := "cache:cachetest:" + strings.ReplaceAll(t.Name(), "/", "_") + ":" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), key).Err()
		_ = rdb.Close()
	})
	return NewExact(rdb, time.Minute), rdb, key
}

func TestExactReturnsWhatWasStoredUntilItExpires(t *testing.T) {
	c, rdb, key := testExact(t)
	ctx := context.Background()

	if e, err := c.Get(ctx, key); e != nil || err != nil {
		t.Fatalf("Get before Put = %+v, %v; want a miss", e, err)
	}
	want := Entry{Response: *answer(choice(0, "hello", finished("stop"))), Vendor: provider.VendorGroq, Model: "llama"}
	if stored, err := c.Put(ctx, key, want); !stored || err != nil {
		t.Fatalf("Put = %v, %v", stored, err)
	}
	got, err := c.Get(ctx, key)
	if err != nil || got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, want)
	}
	if ttl := rdb.TTL(ctx, key).Val(); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("entry TTL = %v, want it to expire within the cache's TTL", ttl)
	}
}

func TestExactLeavesOutAnAnswerThatIsTooLarge(t *testing.T) {
	c, _, key := testExact(t)
	ctx := context.Background()

	big := Entry{Response: *answer(choice(0, strings.Repeat("x", maxEntryBytes), finished("stop")))}
	if stored, err := c.Put(ctx, key, big); stored || err != nil {
		t.Fatalf("Put of an oversized answer = %v, %v; want it left out without an error", stored, err)
	}
	if e, err := c.Get(ctx, key); e != nil || err != nil {
		t.Fatalf("Get = %+v, %v; want a miss", e, err)
	}
}

func TestExactReportsAnEntryItCannotReadBack(t *testing.T) {
	c, rdb, key := testExact(t)
	ctx := context.Background()

	for _, stored := range []string{"not json", `{"response":{"choices":[]},"vendor":"groq","model":"llama"}`} {
		if err := rdb.Set(ctx, key, stored, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		if e, err := c.Get(ctx, key); e != nil || err == nil {
			t.Errorf("Get of %q = %+v, %v; want an error and no entry", stored, e, err)
		}
	}
}
