package cache

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/provider"
)

func testSemantic(t *testing.T) (*Semantic, string) {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL to run the semantic cache's Redis tests")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_TEST_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	ws := "semtest-" + strings.ReplaceAll(t.Name(), "/", "_") + "-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		v := Vector{Family: "mock", Dims: 8}
		vec, age, ans := semanticKeys(ws, v)
		_ = rdb.Del(context.Background(), vec, age, ans).Err()
		_ = rdb.Close()
	})
	return NewSemantic(rdb), ws
}

func unit(xs ...float32) Vector {
	var sum float64
	for _, x := range xs {
		sum += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(sum))
	out := make([]float32, 8)
	for i, x := range xs {
		out[i] = x / n
	}
	return Vector{Family: "mock", Dims: 8, Model: "mock/embed", Values: out}
}

func answerOf(text string) Entry {
	return Entry{
		Response: *answer(choice(0, text, finished("stop"))),
		Vendor:   provider.VendorMock, Model: "echo",
	}
}

func content(t *testing.T, e *Entry) string {
	t.Helper()
	if e == nil {
		t.Fatal("no entry")
	}
	return e.Response.Choices[0].Message.Content
}

func TestSemanticServesACloseQuestionInTheSameContext(t *testing.T) {
	s, ws := testSemantic(t)
	fam := Families["mock"]
	ctx := context.Background()
	if err := s.Put(ctx, ws, fam, "ctx-a", unit(1, 0, 0), answerOf("stored")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Lookup(ctx, ws, fam, "ctx-a", unit(1, 0.1, 0))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if content(t, got) != "stored" || got.Vendor != provider.VendorMock || got.Model != "echo" {
		t.Errorf("got %+v, want the stored answer and its target", got)
	}
}

func TestSemanticMissesWhatItShouldNotServe(t *testing.T) {
	s, ws := testSemantic(t)
	fam := Families["mock"]
	ctx := context.Background()
	if err := s.Put(ctx, ws, fam, "ctx-a", unit(1, 0, 0), answerOf("stored")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for name, c := range map[string]struct {
		workspace string
		ctx       ContextHash
		v         Vector
	}{
		"another context":     {ws, "ctx-b", unit(1, 0, 0)},
		"another workspace":   {ws + "-other", "ctx-a", unit(1, 0, 0)},
		"below the threshold": {ws, "ctx-a", unit(1, 1, 1)},
	} {
		got, err := s.Lookup(ctx, c.workspace, fam, c.ctx, c.v)
		if err != nil || got != nil {
			t.Errorf("%s: got %+v, %v, want a miss", name, got, err)
		}
	}
}

func TestSemanticCapEvictsTheOldest(t *testing.T) {
	s, ws := testSemantic(t)
	s.limit = 2
	clock := time.Now()
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	fam := Families["mock"]
	ctx := context.Background()

	questions := []Vector{unit(1, 0, 0), unit(0, 1, 0), unit(0, 0, 1)}
	for i, q := range questions {
		if err := s.Put(ctx, ws, fam, "ctx", q, answerOf(string(rune('a'+i)))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	if got, err := s.Lookup(ctx, ws, fam, "ctx", questions[0]); err != nil || got != nil {
		t.Errorf("the oldest question: got %+v, %v, want it evicted", got, err)
	}
	for i := 1; i < 3; i++ {
		got, err := s.Lookup(ctx, ws, fam, "ctx", questions[i])
		if err != nil || content(t, got) != string(rune('a'+i)) {
			t.Errorf("question %d: got %+v, %v, want it kept", i, got, err)
		}
	}
	vec, age, ans := semanticKeys(ws, questions[0])
	if n := s.rdb.VCard(ctx, vec).Val(); n != 2 {
		t.Errorf("the vector set holds %d, want 2", n)
	}
	if n := s.rdb.ZCard(ctx, age).Val(); n != 2 {
		t.Errorf("the age set holds %d, want 2", n)
	}
	if n := s.rdb.HLen(ctx, ans).Val(); n != 2 {
		t.Errorf("the answer hash holds %d, want 2", n)
	}
}

func TestSemanticPutOfTheSameQuestionReplacesIt(t *testing.T) {
	s, ws := testSemantic(t)
	fam := Families["mock"]
	ctx := context.Background()
	for _, text := range []string{"first", "second"} {
		if err := s.Put(ctx, ws, fam, "ctx", unit(1, 0, 0), answerOf(text)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got, err := s.Lookup(ctx, ws, fam, "ctx", unit(1, 0, 0))
	if err != nil || content(t, got) != "second" {
		t.Errorf("got %+v, %v, want the second answer", got, err)
	}
	_, _, ans := semanticKeys(ws, unit(1))
	if n := s.rdb.HLen(ctx, ans).Val(); n != 1 {
		t.Errorf("the answer hash holds %d, want 1", n)
	}
}

func TestHashContextIgnoresOnlyTheLastMessage(t *testing.T) {
	hash := func(body string) ContextHash {
		h, err := HashContext(testRoute, request(t, body))
		if err != nil {
			t.Fatalf("HashContext: %v", err)
		}
		return h
	}
	base := hash(`{"model":"m","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"what is Go"}]}`)
	if got := hash(`{"model":"m","stream":true,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"tell me about Go"}]}`); got != base {
		t.Error("a different final question changed the context")
	}
	for name, body := range map[string]string{
		"system prompt": `{"model":"m","messages":[{"role":"system","content":"be long"},{"role":"user","content":"what is Go"}]}`,
		"temperature":   `{"model":"m","temperature":0.3,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"what is Go"}]}`,
		"history":       `{"model":"m","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"what is Go"}]}`,
	} {
		if hash(body) == base {
			t.Errorf("a different %s kept the same context", name)
		}
	}
}
