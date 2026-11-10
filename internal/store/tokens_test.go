package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// tokenKind lets one test body cover both token tables, so a divergence
// between them shows up as a failure in exactly one subtest.
type tokenKind struct {
	name  string
	table string
	issue func(st *Store, userID string, hash []byte, expires time.Time) error
}

var tokenKinds = []tokenKind{
	{
		name:  "verification",
		table: "email_verification_tokens",
		issue: func(st *Store, userID string, hash []byte, expires time.Time) error {
			return st.CreateVerificationToken(context.Background(), userID, hash, expires)
		},
	},
	{
		name:  "reset",
		table: "password_reset_tokens",
		issue: func(st *Store, userID string, hash []byte, expires time.Time) error {
			return st.CreatePasswordResetToken(context.Background(), userID, hash, expires)
		},
	},
}

// consume spends a token of the given kind. For a reset the password argument
// is applied; for verification it is ignored.
func (k tokenKind) consume(st *Store, hash []byte, password string) error {
	ctx := context.Background()
	var err error
	if k.table == "email_verification_tokens" {
		_, err = st.ConsumeVerificationToken(ctx, hash)
	} else {
		_, err = st.ConsumePasswordResetToken(ctx, hash, password)
	}
	return err
}

func TestIssuingATokenReplacesTheOutstandingOne(t *testing.T) {
	for _, k := range tokenKinds {
		t.Run(k.name, func(t *testing.T) {
			st := newTestStore(t)
			res := signup(t, st, "user@example.com")
			first, second := hashOf("first"), hashOf("second")
			soon := time.Now().Add(time.Hour)

			if err := k.issue(st, res.UserID, first, soon); err != nil {
				t.Fatalf("issue first: %v", err)
			}
			if err := k.issue(st, res.UserID, second, soon); err != nil {
				t.Fatalf("issue second: %v", err)
			}

			if got := count(t, st, k.table); got != 1 {
				t.Fatalf("rows = %d, want 1: re-issuing must replace, not accumulate, outstanding links", got)
			}
			if err := k.consume(st, first, "pw"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("first token after re-issue: err = %v, want ErrNotFound: an old emailed link must stop working", err)
			}
			if err := k.consume(st, second, "pw"); err != nil {
				t.Fatalf("second token: %v", err)
			}
		})
	}

	t.Run("tokens of different users do not replace each other", func(t *testing.T) {
		for _, k := range tokenKinds {
			st := newTestStore(t)
			a := signup(t, st, "a@example.com")
			b := signup(t, st, "b@example.com")
			soon := time.Now().Add(time.Hour)
			if err := k.issue(st, a.UserID, hashOf("a"), soon); err != nil {
				t.Fatal(err)
			}
			if err := k.issue(st, b.UserID, hashOf("b"), soon); err != nil {
				t.Fatal(err)
			}
			if err := k.consume(st, hashOf("a"), "pw"); err != nil {
				t.Errorf("%s: user A's token was lost when user B asked for one: %v", k.name, err)
			}
		}
	})

	t.Run("a spent token is not replaced", func(t *testing.T) {
		for _, k := range tokenKinds {
			st := newTestStore(t)
			res := signup(t, st, "user@example.com")
			if err := k.issue(st, res.UserID, hashOf("spent"), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := k.consume(st, hashOf("spent"), "pw"); err != nil {
				t.Fatal(err)
			}
			if err := k.issue(st, res.UserID, hashOf("fresh"), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if got := count(t, st, k.table); got != 2 {
				t.Errorf("%s: rows = %d, want 2: the spent row is the record that the link was used", k.name, got)
			}
		}
	})
}

func TestExpiredTokensAreRefused(t *testing.T) {
	for _, k := range tokenKinds {
		t.Run(k.name, func(t *testing.T) {
			st := newTestStore(t)
			res := signup(t, st, "user@example.com")
			if err := k.issue(st, res.UserID, hashOf("old"), time.Now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := k.consume(st, hashOf("old"), "pw"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound: an expired link must not work", err)
			}
		})
	}
}

func TestConsumeVerificationToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	res := signup(t, st, "user@example.com")

	if u, _ := st.UserByID(ctx, res.UserID); u.EmailVerified() {
		t.Fatal("a new account starts verified")
	}
	if err := st.CreateVerificationToken(ctx, res.UserID, hashOf("v"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	userID, err := st.ConsumeVerificationToken(ctx, hashOf("v"))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if userID != res.UserID {
		t.Errorf("returned user %s, want %s", userID, res.UserID)
	}
	u, err := st.UserByID(ctx, res.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.EmailVerified() {
		t.Fatal("the address is not marked verified after consuming its token")
	}
	verifiedAt := *u.EmailVerifiedAt

	if _, err := st.ConsumeVerificationToken(ctx, hashOf("v")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second use: err = %v, want ErrNotFound: a verification link must be single-use", err)
	}
	if u, _ := st.UserByID(ctx, res.UserID); !u.EmailVerifiedAt.Equal(verifiedAt) {
		t.Error("a refused second use moved the verification time")
	}
	if _, err := st.ConsumeVerificationToken(ctx, hashOf("never-issued")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: err = %v, want ErrNotFound", err)
	}
}

func TestConsumePasswordResetToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	res := signup(t, st, "user@example.com")
	_, h1 := newSession(t, st, res, "s1")
	_, h2 := newSession(t, st, res, "s2")

	if err := st.CreatePasswordResetToken(ctx, res.UserID, hashOf("r"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	userID, err := st.ConsumePasswordResetToken(ctx, hashOf("r"), "reset-hash")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if userID != res.UserID {
		t.Errorf("returned user %s, want %s", userID, res.UserID)
	}
	if got := passwordHash(t, st, res.UserID); got != "reset-hash" {
		t.Errorf("password hash = %q, want reset-hash", got)
	}
	if alive(t, st, h1) || alive(t, st, h2) {
		t.Fatal("a session survived a password reset: a reset is the recovery path for a compromised account")
	}

	t.Run("a second use changes nothing", func(t *testing.T) {
		_, fresh := newSession(t, st, res, "after-reset")

		_, err := st.ConsumePasswordResetToken(ctx, hashOf("r"), "attacker-hash")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: a reset link must be single-use", err)
		}
		if got := passwordHash(t, st, res.UserID); got != "reset-hash" {
			t.Errorf("password hash = %q: a replayed link overwrote the password", got)
		}
		if !alive(t, st, fresh) {
			t.Error("a replayed link revoked a session created after the reset")
		}
	})
}

func TestDeleteStaleTokens(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Rows are keyed by distinct users because issuing replaces a user's
	// unused token.
	live := map[string][]byte{}
	for _, k := range tokenKinds {
		future := time.Now().Add(24 * time.Hour)
		users := map[string]SignupResult{}
		for _, who := range []string{"expired", "old", "recent", "live"} {
			users[who] = signup(t, st, k.name+"-"+who+"@example.com")
		}

		if err := k.issue(st, users["expired"].UserID, hashOf(k.name+"-expired"), time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		for _, who := range []string{"old", "recent"} {
			h := hashOf(k.name + "-" + who)
			if err := k.issue(st, users[who].UserID, h, future); err != nil {
				t.Fatal(err)
			}
			if err := k.consume(st, h, "pw"); err != nil {
				t.Fatal(err)
			}
		}
		live[k.table] = hashOf(k.name + "-live")
		if err := k.issue(st, users["live"].UserID, live[k.table], future); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx,
			`UPDATE `+k.table+` SET used_at = now() - interval '2 hours' WHERE token_hash = $1`,
			hashOf(k.name+"-old")); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := st.DeleteStaleTokens(ctx, time.Hour)
	if err != nil {
		t.Fatalf("DeleteStaleTokens: %v", err)
	}
	if removed != 4 {
		t.Errorf("removed = %d, want 4 (an expired and a long-spent row from each table)", removed)
	}

	for _, k := range tokenKinds {
		if got := count(t, st, k.table); got != 2 {
			t.Errorf("%s: rows left = %d, want 2 (the live token and the recently spent one)", k.table, got)
		}
		if err := k.consume(st, live[k.table], "pw"); err != nil {
			t.Errorf("%s: the live unused token was swept: %v", k.table, err)
		}
	}
}
