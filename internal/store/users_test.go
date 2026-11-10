package store

import (
	"context"
	"errors"
	"testing"
)

const unknownUUID = "00000000-0000-0000-0000-000000000001"

func TestCreateUserWithWorkspace(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	t.Run("creates the user, workspace and membership together", func(t *testing.T) {
		res := signup(t, st, "first@example.com")

		if got := count(t, st, "users"); got != 1 {
			t.Fatalf("users = %d, want 1", got)
		}
		if got := count(t, st, "workspaces"); got != 1 {
			t.Fatalf("workspaces = %d, want 1", got)
		}
		member, err := st.IsMember(ctx, res.WorkspaceID, res.UserID)
		if err != nil || !member {
			t.Fatalf("IsMember = %v, %v; want the signing-up user to be a member of their workspace", member, err)
		}
		w, err := st.DefaultWorkspace(ctx, res.UserID)
		if err != nil {
			t.Fatalf("DefaultWorkspace: %v", err)
		}
		if w.OwnerID != res.UserID {
			t.Errorf("OwnerID = %q, want %q: the new workspace must be owned by the user who made it", w.OwnerID, res.UserID)
		}
	})

	t.Run("a duplicate address leaves no rows behind", func(t *testing.T) {
		before := [3]int{count(t, st, "users"), count(t, st, "workspaces"), count(t, st, "workspace_members")}

		_, err := st.CreateUserWithWorkspace(ctx, "first@example.com", testHash, "Second try")
		if !errors.Is(err, ErrEmailTaken) {
			t.Fatalf("err = %v, want ErrEmailTaken", err)
		}

		after := [3]int{count(t, st, "users"), count(t, st, "workspaces"), count(t, st, "workspace_members")}
		if after != before {
			t.Fatalf("rows (users, workspaces, members) went %v -> %v: a refused signup left a user without a workspace or an orphan workspace", before, after)
		}
	})
}

func TestEmailIsCaseInsensitive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	res := signup(t, st, "  A@B.com ")

	t.Run("lookup ignores case and surrounding space", func(t *testing.T) {
		u, err := st.UserByEmail(ctx, "a@b.com")
		if err != nil {
			t.Fatalf("UserByEmail: %v", err)
		}
		if u.ID != res.UserID {
			t.Fatalf("found user %s, want %s: sign-in would miss accounts typed with different capitalisation", u.ID, res.UserID)
		}
		if u.Email != "a@b.com" {
			t.Errorf("stored Email = %q, want it normalised to a@b.com", u.Email)
		}
	})

	t.Run("a duplicate differing only in case is refused", func(t *testing.T) {
		_, err := st.CreateUserWithWorkspace(ctx, "a@B.COM", testHash, "dup")
		if !errors.Is(err, ErrEmailTaken) {
			t.Fatalf("err = %v, want ErrEmailTaken: two accounts could share one mailbox", err)
		}
	})

	t.Run("the column deduplicates even when a caller skips NormalizeEmail", func(t *testing.T) {
		var n int
		err := st.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = 'A@B.COM'`).Scan(&n)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if n != 1 {
			t.Fatalf("case-different match count = %d, want 1", n)
		}
		_, err = st.pool.Exec(ctx, `INSERT INTO users (email, password_hash) VALUES ('A@b.COM', 'x')`)
		if err == nil {
			t.Fatal("a raw insert differing only in case was accepted: the citext unique index is gone")
		}
	})
}

func TestDefaultWorkspace(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	res := signup(t, st, "owner@example.com")

	w, err := st.DefaultWorkspace(ctx, res.UserID)
	if err != nil {
		t.Fatalf("DefaultWorkspace: %v", err)
	}
	if w.ID != res.WorkspaceID {
		t.Errorf("ID = %s, want %s", w.ID, res.WorkspaceID)
	}
	if w.Name != "Workspace of owner@example.com" {
		t.Errorf("Name = %q: login builds its response from this, so an empty name shows up in the dashboard", w.Name)
	}

	if _, err := st.pool.Exec(ctx, `DELETE FROM workspace_members WHERE user_id = $1`, res.UserID); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	if _, err := st.DefaultWorkspace(ctx, res.UserID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for a user with no membership", err)
	}
}

func TestChangePasswordAndRevokeSessions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	res := signup(t, st, "user@example.com")
	keep, keepHash := newSession(t, st, res, "keep")
	_, otherHash1 := newSession(t, st, res, "other-1")
	_, otherHash2 := newSession(t, st, res, "other-2")

	revoked, err := st.ChangePasswordAndRevokeSessions(ctx, res.UserID, "new-hash", keep.ID)
	if err != nil {
		t.Fatalf("ChangePasswordAndRevokeSessions: %v", err)
	}
	if revoked != 2 {
		t.Errorf("revoked = %d, want 2", revoked)
	}
	if got := passwordHash(t, st, res.UserID); got != "new-hash" {
		t.Errorf("password hash = %q, want the new one", got)
	}
	if !alive(t, st, keepHash) {
		t.Error("the excepted session was revoked: the caller would be signed out of the device they changed the password on")
	}
	if alive(t, st, otherHash1) || alive(t, st, otherHash2) {
		t.Error("another session survived a password change")
	}

	t.Run("an empty exception revokes everything", func(t *testing.T) {
		n, err := st.ChangePasswordAndRevokeSessions(ctx, res.UserID, "newer-hash", "")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if n != 1 || alive(t, st, keepHash) {
			t.Errorf("revoked = %d, kept alive = %v; want 1 revoked and the last session dead", n, alive(t, st, keepHash))
		}
	})

	t.Run("an unknown user is ErrNotFound and revokes nothing", func(t *testing.T) {
		bystander := signup(t, st, "bystander@example.com")
		_, h := newSession(t, st, bystander, "bystander")

		n, err := st.ChangePasswordAndRevokeSessions(ctx, unknownUUID, "x", "")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if n != 0 || !alive(t, st, h) {
			t.Errorf("revoked = %d, bystander alive = %v; a failed change must not touch anyone's sessions", n, alive(t, st, h))
		}
		if got := passwordHash(t, st, bystander.UserID); got != testHash {
			t.Errorf("bystander password hash changed to %q", got)
		}
	})
}
