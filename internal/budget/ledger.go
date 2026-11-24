package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/store"
)

// Action and Limit are the store's, so a key's budget and a workspace's come
// out of Postgres already in this shape.
type (
	Action = store.BudgetAction
	Limit  = store.Budget
)

const (
	Block = store.BudgetBlock
	Warn  = store.BudgetWarn
)

// Scope is whose spend a ledger entry counts. It is part of a Redis key and
// of the refusal a client reads.
type Scope string

const (
	ScopeWorkspace Scope = "workspace"
	ScopeKey       Scope = "key"
)

// Account is one spend counter and the budget on it, nil for none. Spend is
// counted whether or not there is a budget, so one set mid-month starts from
// what was already spent.
type Account struct {
	Scope Scope
	ID    string
	Limit *Limit
}

// Period is a UTC calendar month, as "2026-11".
type Period string

func PeriodOf(t time.Time) Period { return Period(t.UTC().Format("2006-01")) }

// ResetsAt is the first instant of the next period.
func (p Period) ResetsAt() time.Time {
	start, err := time.Parse("2006-01", string(p))
	if err != nil {
		panic("budget: malformed period " + string(p))
	}
	return start.AddDate(0, 1, 0)
}

// keep is how long a period's counters outlive it, so last month's spend can
// still be read for a while after it resets.
const keep = 7 * 24 * time.Hour

// thresholds are the percentages of a budget at which the owner is mailed.
var thresholds = []int64{80, 100}

// reached reports whether spent is at pct percent of the limit. Any spend
// reaches every threshold of a zero budget.
func reached(spent, limit, pct int64) bool {
	return spent > 0 && spent*100 >= pct*limit
}

// Verdict is what the budgets say about a request before it is routed.
type Verdict struct {
	// Known is set when spend was read; Remaining is then the least left
	// across the budgets, never below zero.
	Known     bool
	Remaining int64
	// Exceeded is the first blocking budget already spent, which refuses the
	// request.
	Exceeded *Account
}

// Contacts finds who to mail about a budget. *store.Store is the production one.
type Contacts interface {
	BudgetContact(ctx context.Context, workspaceID, keyID string) (email, keyName string, err error)
}

// Ledger counts each account's spend per period in Redis and mails the
// workspace owner as a budget fills.
type Ledger struct {
	rdb      *redis.Client
	mailer   auth.Mailer
	contacts Contacts
	logger   *slog.Logger
	// prefix namespaces the keys, so tests can keep to their own.
	prefix string
	now    func() time.Time
	// retryAt is when Check may read Redis again after it failed, in unix
	// nanoseconds.
	retryAt atomic.Int64
}

// NewLedger builds a Ledger. A nil logger is slog.Default().
func NewLedger(rdb *redis.Client, mailer auth.Mailer, contacts Contacts, logger *slog.Logger) *Ledger {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ledger{rdb: rdb, mailer: mailer, contacts: contacts, logger: logger, now: time.Now}
}

func (l *Ledger) spendKey(a Account, p Period) string {
	return l.prefix + "spend:" + string(a.Scope) + ":" + a.ID + ":" + string(p)
}

func (l *Ledger) mailKey(a Account, p Period, pct int64) string {
	return l.prefix + "budgetmail:" + string(a.Scope) + ":" + a.ID + ":" + string(p) + ":" + strconv.FormatInt(pct, 10)
}

// Period is the period it is now.
func (l *Ledger) Period() Period { return PeriodOf(l.now()) }

// Spent reads each account's spend this period, in one round trip.
func (l *Ledger) Spent(ctx context.Context, accounts []Account) ([]int64, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	p := l.Period()
	keys := make([]string, len(accounts))
	for i, a := range accounts {
		keys[i] = l.spendKey(a, p)
	}
	vals, err := l.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read budget spend: %w", err)
	}
	out := make([]int64, len(vals))
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if out[i], err = strconv.ParseInt(s, 10, 64); err != nil {
			return nil, fmt.Errorf("read budget spend: %w", err)
		}
	}
	return out, nil
}

const (
	// checkTimeout bounds the read Check makes. A slow counter is treated as
	// one that is down: the request should not wait on it.
	checkTimeout = 250 * time.Millisecond
	// checkCoolOff is how long Check admits without reading after a failed
	// read, so an outage does not add checkTimeout to every request.
	checkCoolOff = 5 * time.Second
)

// Check reads the spend of the accounts with a budget. A Redis error admits
// the request: a budget guards the workspace's own money, as the /v1 rate
// limits do, and an outage of the counter should not take the gateway down.
func (l *Ledger) Check(ctx context.Context, accounts []Account) Verdict {
	var limited []Account
	for _, a := range accounts {
		if a.Limit != nil {
			limited = append(limited, a)
		}
	}
	if len(limited) == 0 {
		return Verdict{}
	}
	if time.Now().UnixNano() < l.retryAt.Load() {
		return Verdict{}
	}
	readCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	spent, err := l.Spent(readCtx, limited)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			l.retryAt.Store(time.Now().Add(checkCoolOff).UnixNano())
		}
		l.logger.WarnContext(ctx, "budget check failed, request admitted", slog.String("error", err.Error()))
		return Verdict{}
	}
	v := Verdict{Known: true, Remaining: -1}
	for i, a := range limited {
		left := max(0, a.Limit.MicroUSD-spent[i])
		if v.Remaining < 0 || left < v.Remaining {
			v.Remaining = left
		}
		if v.Exceeded == nil && a.Limit.Action == Block && spent[i] >= a.Limit.MicroUSD {
			v.Exceeded = &limited[i]
		}
	}
	return v
}

// mailTimeout bounds one budget mail, sent after the request is long done. A
// variable so a test can shorten it.
var mailTimeout = 30 * time.Second

// markTimeout bounds clearing the mark of a mail that was not sent.
const markTimeout = 2 * time.Second

// Add counts micros against every account this period, then mails the owner
// for each threshold a budget has reached, once per threshold and period
// however many instances add at once: SET NX decides which add sends it.
func (l *Ledger) Add(ctx context.Context, workspaceID string, accounts []Account, micros int64) error {
	if micros <= 0 || len(accounts) == 0 {
		return nil
	}
	p := l.Period()
	expires := p.ResetsAt().Add(keep)
	pipe := l.rdb.TxPipeline()
	totals := make([]*redis.IntCmd, len(accounts))
	for i, a := range accounts {
		key := l.spendKey(a, p)
		totals[i] = pipe.IncrBy(ctx, key, micros)
		pipe.ExpireAt(ctx, key, expires)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("add budget spend: %w", err)
	}

	var errs []error
	for i, a := range accounts {
		if a.Limit == nil {
			continue
		}
		spent := totals[i].Val()
		for _, pct := range thresholds {
			if !reached(spent, a.Limit.MicroUSD, pct) {
				continue
			}
			key := l.mailKey(a, p, pct)
			first, err := l.rdb.SetNX(ctx, key, 1, time.Until(expires)).Result()
			if err != nil {
				errs = append(errs, fmt.Errorf("mark budget mail: %w", err))
				continue
			}
			if first {
				go l.mail(workspaceID, a, p, pct, spent, key)
			}
		}
	}
	return errors.Join(errs...)
}

// mail tells the owner a budget reached pct. When it cannot be sent its mark
// is removed, so the next request past the threshold tries again.
func (l *Ledger) mail(workspaceID string, a Account, p Period, pct, spent int64, mark string) {
	ctx, cancel := context.WithTimeout(context.Background(), mailTimeout)
	defer cancel()
	err := l.sendMail(ctx, workspaceID, a, p, pct, spent)
	if err == nil {
		return
	}
	l.logger.Warn("budget mail not sent", slog.String("error", err.Error()))
	// The send may have used up ctx, so the mark gets a context of its own:
	// a mark left behind loses this month's mail.
	delCtx, cancelDel := context.WithTimeout(context.Background(), markTimeout)
	defer cancelDel()
	if err := l.rdb.Del(delCtx, mark).Err(); err != nil {
		l.logger.Warn("budget mail mark not cleared", slog.String("error", err.Error()))
	}
}

func (l *Ledger) sendMail(ctx context.Context, workspaceID string, a Account, p Period, pct, spent int64) error {
	keyID := ""
	if a.Scope == ScopeKey {
		keyID = a.ID
	}
	email, keyName, err := l.contacts.BudgetContact(ctx, workspaceID, keyID)
	if err != nil {
		return err
	}
	return l.mailer.Send(ctx, message(email, keyName, a, p, pct, spent))
}

func message(to, keyName string, a Account, p Period, pct, spent int64) auth.Message {
	whose := "Your workspace's monthly budget"
	if a.Scope == ScopeKey {
		whose = fmt.Sprintf("The monthly budget of your gateway key %q", keyName)
	}
	state := "has reached 80%"
	if pct >= 100 {
		state = "is spent"
	}
	effect := "Requests are still served: this budget only warns."
	if a.Limit.Action == Block {
		effect = "Requests that would call a provider are now refused, until the budget resets or is raised."
		if pct < 100 {
			effect = "At 100%, requests that would call a provider will be refused."
		}
	}
	return auth.Message{
		To:      to,
		Subject: whose + " " + state,
		Body: fmt.Sprintf("%s %s: %s of %s spent in %s.\n\n%s\n\nThe budget resets on %s (UTC).\n",
			whose, state, USD(spent), USD(a.Limit.MicroUSD), p, effect, p.ResetsAt().Format("2 January 2006")),
	}
}

// USD formats micro-USD as dollars, to the cent unless that hides the amount.
func USD(micros int64) string {
	if micros != 0 && micros%10_000 != 0 {
		return "$" + strconv.FormatFloat(float64(micros)/1e6, 'f', 6, 64)
	}
	return "$" + strconv.FormatFloat(float64(micros)/1e6, 'f', 2, 64)
}
