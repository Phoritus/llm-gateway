package budget

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

type mailbox struct {
	mu   sync.Mutex
	sent []auth.Message
	fail bool
	// hang makes Send wait until its context ends, as a relay that never
	// answers does.
	hang bool
}

func (m *mailbox) Send(ctx context.Context, msg auth.Message) error {
	m.mu.Lock()
	hang := m.hang
	m.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("relay down")
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mailbox) messages() []auth.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]auth.Message(nil), m.sent...)
}

// waitFor waits for n mails, then a moment longer so that one too many shows.
func (m *mailbox) waitFor(t *testing.T, n int) []auth.Message {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && len(m.messages()) < n; {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	got := m.messages()
	if len(got) != n {
		t.Fatalf("sent %d mails, want %d: %+v", len(got), n, got)
	}
	return got
}

type owner struct{}

func (owner) BudgetContact(_ context.Context, _, keyID string) (string, string, error) {
	if keyID != "" {
		return "owner@example.com", "ci", nil
	}
	return "owner@example.com", "", nil
}

// testLedger is a ledger over REDIS_TEST_URL whose keys are the test's own,
// deleted afterwards. No test may FLUSHDB: other packages share the database.
func testLedger(t *testing.T) (*Ledger, *mailbox) {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL to run the ledger tests")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	mail := &mailbox{}
	l := NewLedger(rdb, mail, owner{}, nil)
	l.prefix = "budgettest:" + t.Name() + ":"
	l.now = func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() {
		ctx := context.Background()
		keys, _ := rdb.Keys(ctx, l.prefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
		_ = rdb.Close()
	})
	return l, mail
}

func TestConcurrentAddsMailOncePerThreshold(t *testing.T) {
	l, mail := testLedger(t)
	ctx := context.Background()
	ws := Account{Scope: ScopeWorkspace, ID: "ws1", Limit: &Limit{MicroUSD: 1000, Action: Warn}}
	key := Account{Scope: ScopeKey, ID: "k1"}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if err := l.Add(ctx, "ws1", []Account{ws, key}, 40); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	spent, err := l.Spent(ctx, []Account{ws, key})
	if err != nil {
		t.Fatal(err)
	}
	if spent[0] != 2000 || spent[1] != 2000 {
		t.Errorf("spent = %v, want 2000 for both accounts", spent)
	}
	got := mail.waitFor(t, 2)
	subjects := got[0].Subject + " | " + got[1].Subject
	if !strings.Contains(subjects, "reached 80%") || !strings.Contains(subjects, "is spent") {
		t.Errorf("subjects = %q, want one at 80%% and one at 100%%", subjects)
	}

	ttl, err := l.rdb.TTL(ctx, l.spendKey(ws, "2026-11")).Result()
	if err != nil || ttl < 13*24*time.Hour {
		t.Errorf("spend expires in %s (%v), want past the month's end plus a week", ttl, err)
	}
}

func TestAKeyBudgetMailNamesTheKey(t *testing.T) {
	l, mail := testLedger(t)
	key := Account{Scope: ScopeKey, ID: "k1", Limit: &Limit{MicroUSD: 0, Action: Block}}
	if err := l.Add(context.Background(), "ws1", []Account{key}, 1); err != nil {
		t.Fatal(err)
	}
	got := mail.waitFor(t, 2)
	if !strings.Contains(got[0].Subject, `"ci"`) || got[0].To != "owner@example.com" {
		t.Errorf("mail = %+v", got[0])
	}
}

func TestAMailThatFailsIsTriedAgainOnTheNextAdd(t *testing.T) {
	l, mail := testLedger(t)
	ctx := context.Background()
	ws := Account{Scope: ScopeWorkspace, ID: "ws1", Limit: &Limit{MicroUSD: 100, Action: Block}}
	mail.fail = true
	if err := l.Add(ctx, "ws1", []Account{ws}, 90); err != nil {
		t.Fatal(err)
	}
	mail.waitFor(t, 0)

	mail.mu.Lock()
	mail.fail = false
	mail.mu.Unlock()
	if err := l.Add(ctx, "ws1", []Account{ws}, 1); err != nil {
		t.Fatal(err)
	}
	mail.waitFor(t, 1)
}

func TestCheckRefusesOnlyASpentBlockingBudget(t *testing.T) {
	l, _ := testLedger(t)
	ctx := context.Background()
	warn := Account{Scope: ScopeWorkspace, ID: "ws1", Limit: &Limit{MicroUSD: 500, Action: Warn}}
	block := Account{Scope: ScopeKey, ID: "k1", Limit: &Limit{MicroUSD: 2000, Action: Block}}
	if err := l.Add(ctx, "ws1", []Account{warn, block}, 900); err != nil {
		t.Fatal(err)
	}

	v := l.Check(ctx, []Account{warn, block})
	if !v.Known || v.Exceeded != nil || v.Remaining != 0 {
		t.Errorf("verdict = %+v, want admitted with 0 remaining: the spent budget only warns", v)
	}
	if err := l.Add(ctx, "ws1", []Account{warn, block}, 1100); err != nil {
		t.Fatal(err)
	}
	if v := l.Check(ctx, []Account{warn, block}); v.Exceeded == nil || v.Exceeded.Scope != ScopeKey {
		t.Errorf("verdict = %+v, want refused by the key's budget", v)
	}
	if v := l.Check(ctx, []Account{{Scope: ScopeWorkspace, ID: "ws1"}}); v.Known {
		t.Errorf("verdict without a budget = %+v, want none", v)
	}
}

func TestCheckAdmitsWhenRedisFails(t *testing.T) {
	l, _ := testLedger(t)
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = broken.Close() })
	l.rdb = broken
	v := l.Check(context.Background(), []Account{{Scope: ScopeKey, ID: "k1", Limit: &Limit{Action: Block}}})
	if v.Known || v.Exceeded != nil {
		t.Errorf("verdict = %+v, want admitted with the spend unknown", v)
	}
}

func TestAMailThatTimesOutIsTriedAgainOnTheNextAdd(t *testing.T) {
	l, mail := testLedger(t)
	defer func(d time.Duration) { mailTimeout = d }(mailTimeout)
	mailTimeout = 50 * time.Millisecond
	ctx := context.Background()
	ws := Account{Scope: ScopeWorkspace, ID: "ws1", Limit: &Limit{MicroUSD: 100, Action: Warn}}
	mail.hang = true
	if err := l.Add(ctx, "ws1", []Account{ws}, 90); err != nil {
		t.Fatal(err)
	}
	mark := l.mailKey(ws, "2026-11", 80)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		n, err := l.rdb.Exists(ctx, mark).Result()
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the mark of a mail that timed out was never cleared, so this month's mail is lost")
		}
	}
}

// hungRedis accepts connections and never answers on them.
func hungRedis(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

func TestCheckDoesNotWaitOnAHungRedis(t *testing.T) {
	opts, err := store.RedisOptions("redis://" + hungRedis(t))
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	l := NewLedger(rdb, &mailbox{}, owner{}, nil)
	accounts := []Account{{Scope: ScopeKey, ID: "k1", Limit: &Limit{Action: Block}}}

	start := time.Now()
	if v := l.Check(context.Background(), accounts); v.Known || v.Exceeded != nil {
		t.Errorf("verdict = %+v, want admitted with the spend unknown", v)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the first check took %s, want it bounded well under a second", took)
	}
	start = time.Now()
	l.Check(context.Background(), accounts)
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Errorf("a check right after a failure took %s, want it to skip Redis", took)
	}
}
