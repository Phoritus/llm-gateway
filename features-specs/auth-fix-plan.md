# Plan: findings 4, 6, 7, 8

Implements four open items from [auth-fix-list.md](auth-fix-list.md). Finding 5 is already
closed (the members feature was deleted), so "4–7" here means 4, 6 and 7.

## Context

Three of these are holes the review found and one is a policy that was never written
down. They are planned together because **finding 8 changes the stakes of finding 6**:
once a user cannot use the service until they confirm their address, a mailer that
only writes to the log is no longer a development convenience — it is a service that
nobody can sign up for. So the mailer gets fixed first, and gets fixed properly.

Decisions taken (2026-11-10):

- **Finding 8 — account hygiene stays open.** Login succeeds and issues a session for
  an unverified account. `/me`, logout, logout-all and change-password keep working;
  every feature route returns 403 `email_not_verified`.
- **Finding 6 — a real SMTP mailer now**, not just a refusal to start. Production that
  cannot boot would block finding 8 from ever shipping.

## 1. Finding 7 — bind the dependencies to loopback

One line each, plus the test database that has the same problem.

- [docker-compose.yml:13](../docker-compose.yml#L13) → `"127.0.0.1:${POSTGRES_PORT:-5432}:5432"`
- [docker-compose.yml:27](../docker-compose.yml#L27) → `"127.0.0.1:${REDIS_PORT:-6379}:6379"`
- [scripts/test.sh:47](../scripts/test.sh#L47) → `-p "127.0.0.1:${TEST_PG_PORT}:5432"` — the
  throwaway integration Postgres publishes on `0.0.0.0` today for the same reason.

Keep the existing comments about port collisions; add one line saying why loopback.
`scripts/test.sh` connects over `127.0.0.1` already, so nothing else moves.

**Drive-by in the same file:** the comment at [scripts/test.sh:67](../scripts/test.sh#L67)
says the integration tests run `FLUSHDB`. They do not, and must not — `CLAUDE.md`
records the intermittent failure that rule exists to prevent. Correct the comment to
say they use a separate logical database and delete only their own keys.

## 2. Finding 4 — make the password change atomic

Add one store method, modelled directly on
[`ConsumePasswordResetToken`](../internal/store/tokens.go#L99-L138), which already does
this same pair of writes in one transaction:

```go
// internal/store/users.go
func (s *Store) ChangePasswordAndRevokeSessions(
    ctx context.Context, userID, newPasswordHash, exceptSessionID string,
) (int64, error)
```

Inside one `tx`: `UPDATE users SET password_hash …` (zero rows affected → `ErrNotFound`),
then the sessions update, reusing the `$2::uuid IS NULL OR id <> $2::uuid` form and the
`except any` nil-versus-string handling from
[`RevokeUserSessions`](../internal/store/sessions.go#L85-L104) — the comment there about
Postgres not promising to short-circuit `OR` applies unchanged. Return the revoked count,
then commit.

Then:

- [`auth.ChangePassword`](../internal/auth/tokens.go#L103-L125) calls that one method in
  place of the two sequential calls.
- **Delete `UpdatePasswordHash`** ([users.go:122](../internal/store/users.go#L122)) — it has
  no other caller, so leaving it would re-offer the non-atomic path to the next person.
  This also removes one item from finding 11.
- Keep `RevokeUserSessions`: `handleLogoutAll` still uses it
  ([auth.go:251](../internal/api/auth.go#L251)).

**On testing this honestly:** atomicity cannot be proven without fault injection
between the two writes, which is not worth a test harness here. The existing
change-password integration test guards the behaviour; the guarantee comes from the
single transaction being visible in one function.

## 3. Finding 6 — an SMTP mailer, and fail closed without one

**New `internal/auth/smtp.go`** implementing the existing
[`Mailer`](../internal/auth/mail.go#L11-L13) interface, on `net/smtp`:

- `smtp://` → STARTTLS (port 587, what Resend, Brevo and Mailgun want);
  `smtps://` → implicit TLS (port 465), which needs `tls.Dial` + `smtp.NewClient`
  because `smtp.SendMail` cannot do it.
- TLS is required unless the host is loopback, so a local MailHog still works.
- A `buildMessage` helper writes the RFC 5322 headers (From, To, Subject, Date,
  MIME-Version, `text/plain; charset=utf-8`) and **strips CR and LF from From, To and
  Subject**. The address is validated upstream, but header injection is the one bug
  class that turns a mailer into an open relay, so it is refused at the point of use.
- Dial and connection deadlines from the caller's context, 10s cap.

**`internal/config/config.go`** gains `SMTPURL`, parsed and validated at `Load`. Under
`APP_ENV=production`, three things become required, each with its own named problem in
the existing multi-problem error:

- `SMTP_URL` — absent, the only mailer is `LogMailer`, which is the finding.
- `MAIL_FROM` — the `no-reply@localhost` default is rejected by every real relay.
- `PUBLIC_BASE_URL` — its `http://localhost:8080` default silently sends links that
  point at the user's own machine. Same class of bug, found while reading for this one.

**`cmd/gateway/main.go`** picks `SMTPMailer` when `SMTPURL` is set, else `LogMailer`
with the existing development warning. Production cannot reach the `LogMailer` branch,
because `Load` refuses first — which makes the claim already written at
[mail.go:28](../internal/auth/mail.go#L28) ("it is never the production mailer") true
instead of contradicted.

**`.env.example`** documents `SMTP_URL` and replaces the "no provider is wired yet" note.

**Tests:** a new `internal/config/config_test.go` — the first test in that package, which
starts paying down finding 18. Table-driven over `t.Setenv`: production without
`SMTP_URL` fails and names it; with it, loads; a malformed URL fails. Plus a unit test
for `buildMessage` covering the header-injection strip.

## 4. Finding 8 — enforce email verification

**Middleware** in [internal/auth/middleware.go](../internal/auth/middleware.go), beside
`RequireSession`:

```go
func RequireVerifiedEmail(onUnverified ErrorFunc) func(http.Handler) http.Handler
```

Reads `IdentityFrom`; passes only when `EmailVerifiedAt != nil`. A context with **no**
identity is refused too — it means the middleware was mounted without `RequireSession`
above it, and denying is the safe reading of a wiring mistake.

**Error** in [internal/api/errors.go](../internal/api/errors.go): `emailNotVerified` →
403, `permission_error`, code `email_not_verified`, with a message that tells the user
to check their inbox or request a new link.

**Routes** in [routes.go:79-91](../internal/api/routes.go#L79-L91). The authenticated
group keeps `/me`, `/auth/change-password` and `/auth/logout-all`, and gains a nested
group:

```go
r.Group(func(r chi.Router) {
    r.Use(auth.RequireVerifiedEmail(emailNotVerified))
    // Gateway keys, provider credentials, routes and usage mount here.
})
```

That group is **empty today** — there are no feature routes yet. It is still the right
shape, for the same reason [`limitPerUser`](../internal/api/routes.go#L83-L85) meters the
group rather than each handler: it makes the gate something a future route inherits
rather than something a future route has to remember.

**Written down**, which is what finding 8 actually asked for: a paragraph in
`CLAUDE.md` under "Already decided" stating the policy and which endpoints are
deliberately exempt, and a line in [auth/doc.go](../internal/auth/doc.go) recording that
gateway-key minting and `/v1` inherit it when they land.

**Tests:**

- Unit test in `internal/auth` driving `RequireVerifiedEmail` with `httptest` and
  `WithIdentity` — verified passes, unverified 403s, no identity 403s. This avoids
  mounting a test-only route in production code to exercise an empty group.
- Integration assertions that `/me` and change-password still answer 200 for an
  unverified session, which is the decision above and the thing most likely to be
  broken by accident later.

No handler changes: login already returns `email_verified` in its body through
[`meResponse`](../internal/api/auth.go#L275-L285), so the dashboard can branch on it.

## Order

`7 → 4 → 6 → 8`, one commit each. 6 before 8 because 8 makes mail load-bearing.

## Verification

- `make check` after each step; `make test-int` before each commit.
- After step 1: `make deps-reset && make deps-up`, then `make psql` and `make redis-cli`
  to confirm both are still reachable from the host, and `docker compose ps` to confirm
  the published address now reads `127.0.0.1`.
- After step 3: start with `APP_ENV=production` and no `SMTP_URL` and confirm the
  refusal names `SMTP_URL`, `MAIL_FROM` and `PUBLIC_BASE_URL`. A real send is a manual
  smoke test against whichever provider gets used — it cannot be covered by CI here.
- After step 4: `make dev`, sign up, take the link from the log, and check `/api/me`
  answers 200 both before and after verifying.

## Risks

- **Loopback bind** breaks any setup where Docker is not on the same host as the
  developer, or a WSL2 arrangement without localhost forwarding. Revert is one line per
  port if it bites.
- **An existing local `.env` with `APP_ENV=production`** will now refuse to start until
  the three variables are set. That is the intended effect, but it will look like a
  regression the first time it happens.
- **The SMTP path cannot be tested end to end in CI.** `buildMessage` is unit tested;
  the dial, STARTTLS and auth path is covered only by the manual smoke test above.
