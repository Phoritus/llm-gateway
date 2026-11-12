# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

The skeleton, the control plane's authentication, and a mock-only data plane
are implemented. Read a package's `doc.go` to learn what belongs in it before
adding files.

| Package | State |
| --- | --- |
| `config`, `store` | Built: env loading, pgx pool, goose migrations, Redis |
| `telemetry` | Built: request id, redacting access log (with `trace_id`), OpenTelemetry setup over OTLP, bounded `http.route` labelling, `postgres.up`/`redis.up` gauges. `store` adds the pool gauges and `ratelimit` the `ratelimit.rejections` counter |
| `auth` | Sessions, argon2id passwords, CSRF, email verification, password reset. **Gateway keys are not written yet** |
| `api` | Auth endpoints, health probes |
| `ratelimit` | Redis token bucket and its Lua script, used so far only by the auth endpoints |
| `provider`, `provider/mock` | The one-method `Provider` interface (`ChatStream` → pull `Stream`), the OpenAI wire types, typed errors, and an `Assembler` that builds a non-streaming response from chunks. The mock is scripted by model name: `mock/echo`, `slow`, `rate-limited`, `fail`, `context-too-long`, `break` |
| `gateway` | `POST /v1/chat/completions`, streaming and not, against a single provider. **Mounted only outside production**: it has no gateway keys yet |
| `router`, `cache`, `crypto`, `usage`, `provider/openai`, `gemini`, `anthropic` | Still a one-line `doc.go` |

Build-order steps 1 and 2 and the auth half of step 3 are done. Next is the
rest of step 3: gateway keys, encrypted provider credentials and routes, which
is also when `/v1` gets authentication and can be mounted in production. The
step 2 design is in `features-specs/mock-provider-v1/design.md`.

The design is already settled and written down. `Backend Architecture BYOK LLM Gateway (Go).md` at the repo root is the authoritative spec — read it before any non-trivial change. It also exists as a live Claude Docs document at https://claude.ai/artifact/KiGTMPuYo84W15uVSzNM1j, which is read through the Claude Docs connector rather than WebFetch. The two can drift; prefer the local file for code decisions.

## Commands

**Every command needed to work on this repo is a `make` target.** Run `make`
with no arguments to list them. When a new step becomes part of starting,
running, testing or releasing the service, add it as a target in `Makefile`
and document it here — a command that only lives in someone's shell history
does not count as a command this project has.

Each target is a one-line delegation to a script in `scripts/`, so the same
work runs without GNU make (`scripts/deps.sh up`, `scripts/run.sh`, …). Put
the logic in the script, not in the Makefile recipe.

From a fresh checkout:

```sh
make setup      # write .env.dev with generated secrets, start Postgres and Redis
make run        # run the service with .env.dev loaded
```

Day to day:

| Command | What it does |
| --- | --- |
| `make` | List every target |
| `make dev` | Start the dependencies, then the service |
| `make run` | Run the service with `.env.dev` loaded (creates it if missing); `ENV_FILE=…` picks another file |
| `make build` | Compile to `bin/gateway` |
| `make deps-up` / `deps-down` | Start or stop Postgres and Redis, waiting for health |
| `make deps-reset` | Stop them and drop the volumes |
| `make psql` / `make redis-cli` | A shell on either dependency |
| `make obs-up` / `obs-down` / `obs-status` | Start, stop or show the local Grafana stack (`grafana/otel-lgtm`, Grafana on `127.0.0.1:3000`, admin/admin). Opt-in: `deps-up` never starts it, and `deps-down` does not stop it |
| `make check` | `fmt-check`, `vet`, `build`, unit tests — must pass before a commit |
| `make test` | Unit tests; the ones needing a database skip |
| `make test-int` | Every test, starting a throwaway Postgres and Redis first |
| `make test-race` | The same under the race detector (needs cgo and a C compiler) |
| `make env-secrets` | Fill in a missing or placeholder `MASTER_KEY`/`SESSION_SECRET` |
| `make docker-build` | Build the container image |

`make` needs bash on `PATH`; on Windows run it from Git Bash. If `make` itself
is missing: `winget install ezwinports.make`.

### Things that catch people out

**The `http.route` label must never be a raw URL path.** `telemetry.Route`
reports a registered pattern (`/healthz`, `/api/me`, chi's `/api/auth/*`) or
the fixed `unmatched`, and `api.routeTelemetry` feeds it chi's pattern, because
`otelhttp` cannot see a sub-router's match. A path with an id in it would mint
a new metric series per id, and Grafana Cloud's free tier accepts about 10k
active series before it drops data. The same goes for any label you add to a
metric: use a value from a fixed set (`ratelimit.rejections` is labelled by
`scope`, a literal at every call site, and never by the id). Telemetry is off
when `OTEL_EXPORTER_OTLP_ENDPOINT` is empty; production logs a warning.

**The first byte to the client is the point of no return, and it is the first
*chunk*, not the provider call.** The `/v1` handler reads the upstream's first
chunk before writing any header, so a provider that accepts the request and
fails on its first read is still answered with a status (and can fail over
once routing lands). After that, failure is a final `data: {"error":…}` event.
A provider must make `Next` return promptly when its context ends: that is what
stops an abandoned generation, and `TestClientDisconnectStopsTheUpstream` holds
it to that.

**`OTEL_EXPORTER_OTLP_HEADERS` is a secret.** For Grafana Cloud it holds the
access token. `config` deliberately does not read it, the OTLP exporters read
it from the environment themselves, and nothing may log it. Set it in the
deployment's own settings, not in a file that is committed or shared, and never
in `.env.prod` on the user's behalf.

**A placeholder `MASTER_KEY` stops the service from starting.** The config
loader requires 32 bytes of base64 and refuses to run without it, by design.
`make env-secrets` generates only that and `SESSION_SECRET`, leaving every
other line of an existing `.env.dev` byte for byte — which matters when it
holds real provider URLs and tokens.

**Rate limits are keyed on the client address, so `TRUSTED_PROXIES` is a
security setting.** `X-Forwarded-For` is believed only when the request's TCP
peer is a trusted proxy: the private ranges always are (no internet client can
reach you from one), loopback deliberately is not, and public proxy ranges —
Cloudflare's, here — come from `TRUSTED_PROXIES`. The two ways to get it wrong
look completely different: a missing range means everyone behind one CDN edge
node shares a bucket and legitimate users get 429s, while a range listed that
is not really a proxy lets anyone inside it choose its own bucket and the limit
stops existing. IPv6 is bucketed per `/64`, not per address, because a single
client is routinely handed the whole prefix.

**`internal/config` is the only place a duration or an origin policy is
decided.** `config.Load` validates every lifetime the service runs on
(`SESSION_TTL`, `SESSION_IDLE_TTL`, `VERIFICATION_TTL`, `RESET_TTL`) and
`auth.Config` defaults none of them, so a zero passed in is a zero at runtime
rather than a quietly substituted default. A second layer of defaults inside
`internal/auth` used to shadow this: dead in production, and the owner of two
values `config` had no field for, which made them documented policy that
nothing could configure. Allowed origins are normalized once, into the
`config.Origins` type, because two readers -- the CORS headers and
`auth.RequireSameOrigin` -- compare against the same list and had drifted.
`ALLOWED_ORIGINS` is required in production: an empty list makes the
same-origin guard refuse every state-changing request a browser sends, so the
control plane answers 403 to its own dashboard.

**Password rules are enforced by `auth.HashPassword`, not by callers.**
Hashing is the only door to a stored password, so signup, reset and change all
pass through it and a path that forgets to validate cannot exist. Handlers
translate the refusal with `auth.IsPasswordRejected` instead of pre-checking;
when they pre-checked, the handler and the service each decided the ordering
and which failure a client saw depended on which layer ran first.

**Development and production read different variable names for their
dependencies.** `APP_ENV=development` reads `DATABASE_URL_LOCAL` and
`REDIS_URL_LOCAL`; `APP_ENV=production` reads `DATABASE_URL` and `REDIS_URL`.
Neither name falls back to the other, so a local and a deployed URL can sit in
the same env file without either one shadowing the other. With one name for both
they competed for the same line, the later one won in silence, and a service
started for local work migrated and wrote to the production database with
nothing in its output to say which it had opened. `load_env` now also refuses
to start when any key appears twice.

**The integration tests' Postgres port can be unbindable with nothing
listening on it.** `TEST_PG_PORT` defaults to 55432, which falls inside a
Hyper-V excluded range on some Windows machines (`netsh int ipv4 show
excludedportrange protocol=tcp`). Nothing is listening, so the script's
free-port check cannot see it and docker's refusal to publish is the only
signal. `scripts/test.sh` now names `TEST_PG_PORT` in that refusal, cleans up
the half-created container, and recreates the container when its published
port no longer matches: `TEST_PG_PORT=55032 make test-int`.

**A Postgres installed on the host shadows the compose container.** Docker's
published port loses that race silently, so connections reach the other server
and fail *authentication* rather than failing to connect. `POSTGRES_PORT` and
`REDIS_PORT` in `.env.dev` set what compose publishes, and `make env` picks free
ports when the defaults are taken. The integration tests sidestep it entirely
by running their own Postgres on `55432`.

### Tests

`make test-int` sets `DATABASE_TEST_URL` and `REDIS_TEST_URL` and manages the
containers. The database-backed tests skip unless both are set, and they
truncate every table, so they must never point at a database anyone cares
about. To run them by hand:

```sh
DATABASE_TEST_URL=postgres://gateway:gateway@127.0.0.1:55432/llm_gateway_test?sslmode=disable \
REDIS_TEST_URL=redis://127.0.0.1:6379/1 \
go test -count=1 ./...
```

They cover the token bucket's Lua script (`internal/ratelimit`), the store's
transaction boundaries and single-use tokens (`internal/store`), and the whole
control plane, including cross-tenant isolation (`internal/api`).

**`internal/store`'s tests create a database of their own**, named after
`DATABASE_TEST_URL`'s with a `_store` suffix, so the test user needs
`CREATE DATABASE` rights. The compose and throwaway containers give it. They
need their own database for the same reason no test may `FLUSHDB`: `internal/api`
truncates every table in the database it is handed, `go test ./...` runs the
packages in parallel, and a truncate there would otherwise land in the middle of
a test here. A single
test: `go test ./internal/auth -run TestVerifyCSRF -v`.

**No test may `FLUSHDB`.** `REDIS_TEST_URL` names one logical database and
`go test ./...` runs the packages in parallel, so a flush in one package lands
in the middle of another's. It did: it refilled a token bucket mid-drain and
`internal/ratelimit` reported one request allowed past its burst, intermittently
and in a package the flushing fixture never touched. Clean up by deleting your
own keys — `internal/api` drops `rl:*`, and the bucket tests key themselves
under `rltest:<test name>`.

## Module path

The module is the bare path `llm-gateway`, so internal imports read `llm-gateway/internal/router`. Publishing this later means changing `go.mod` and every import.

## Architecture

One Go binary serves two planes that share the key, encryption and storage code:

- **Data plane** (`/v1`, `internal/gateway`) — apps authenticate with a gateway key. OpenAI-compatible, streaming, latency-critical.
- **Control plane** (`/api`, `internal/api`) — the dashboard authenticates with a session cookie, rate limited on its own.

Splitting these into separate services later must not require interface changes.

### The request pipeline

`POST /v1/chat/completions` runs seven ordered steps spanning most of `internal/`. Reordering or skipping one is an architectural change, not a refactor:

1. **Authenticate** — SHA-256 the bearer key, look it up in an in-memory cache backed by Postgres, resolve to a workspace
2. **Parse and validate** — decode OpenAI format, enforce size limits, resolve `model` to a workspace-owned route alias
3. **Rate limit** — Redis token bucket per gateway key and per workspace; reserve estimated tokens now
4. **Cache lookup** — exact-match hash in Phase 1; a hit returns immediately and skips the provider
5. **Route and call** — `internal/router` orders targets, then applies skip rules and the circuit breaker
6. **Stream back** — copy provider chunks through, flushing each one
7. **Finish** — reconcile the reservation, store the response if cacheable, push a usage event to a buffered channel

### Invariants that cut across packages

- **Failover is legal only before the first byte reaches the client.** After that, an upstream failure must end the stream with an error event. This makes step 6 a point of no return.
- **Every tenant row and query carries `workspace_id`**, enforced in one data-access layer, and handlers take it from the session — never from the request body.
- **Shared mutable state lives in Redis with a TTL** (buckets, skip flags, breaker state) so all instances agree and expiry is automatic. In-process copies are fine but must stay around a second.
- **Rate limiting reserves then reconciles** — estimate before the call, then add or refund against the provider's reported usage. There is no exact Go tokenizer, so an estimate only has to be good enough to reserve against.
- **Usage logging never delays a response.** Events go to a buffered channel; a background worker batches them into Postgres.
- **Errors use the OpenAI error shape** so SDKs and IDE clients need no special handling. Upstream trouble is never a 500 — exhaustion returns 429 with `Retry-After` if any failure was a limit, 413 if all were context-length errors, otherwise 502.
- **Never cache** tool-call requests, error responses, or workspaces that turned caching off.
- **The request context carries a deadline and is cancelled on client disconnect**, which stops the upstream call so abandoned generations aren't paid for.

### Providers

`internal/provider` defines one interface; the core speaks a single internal shape (OpenAI chat format) and each adapter translates to and from it. Adapters return typed errors (`ErrAuth`, `ErrQuota`, `ErrRateLimited`, `ErrContextTooLong`, `ErrUpstream`, `ErrTimeout`) so the router picks a skip rule without parsing messages.

Most free providers already speak OpenAI format, so `provider/openai` with a configurable base URL covers Groq, Cerebras, OpenRouter and Mistral; Gemini and Anthropic need their own adapters.

`provider/mock` is load-bearing, not a placeholder. It must be able to return slow, rate-limited, failing and mid-stream-broken responses so fallback behavior is testable without spending provider quota.

## Configuration

Environment variables only — see `.env.example`. The service must refuse to start when `MASTER_KEY` is absent. `SEMANTIC_CACHE` and `CONTEXT_HANDOFF` gate Phase 2 features and default off. `.env.dev` holds the local secrets and is gitignored. The scripts read it for local work (`ENV_FILE=<file>` picks another) and refuse `ENV_FILE=.env.prod` by name, so no `make` target can open the production file.

## Phasing

Phase 1 runs on free tiers (Render, Upstash Redis, Neon Postgres) with exact-match caching only. Phase 2 consolidates onto one VPS and adds the semantic cache, context handoff, Prometheus and Grafana, and custom endpoints behind an SSRF guard. Keep Phase 2 work out of Phase 1 paths except behind the flags above.

Build order from the spec: skeleton (config, connections, migrations, health endpoints, request id, logging) → mock provider streaming through `/v1`, the first thing to demo → gateway keys, encrypted credentials, routes, control plane → real adapters → rate limiting, then routing with fallback → usage events and metrics (**Gate A: free launch**) → free-first routing and exact cache → Phase 2 (**Gate B: go live**).

## Already decided — don't re-litigate

The spec's open-decisions table resolves these: semantic cache and context handoff are Phase 2; default max fallback hops is 3, configurable per route; email-and-password sign-in first; `chi` for routing; an Anthropic-native endpoint comes later.

**There are no roles, and `workspace_members` has no role column.** Phase 1
gives a workspace exactly one member — the person who signed up, recorded
again as `workspaces.owner_id` — so a role would have stored a value derived
from `owner_id` and gated nothing but the member-administration endpoints,
which the spec itself places in Phase 2. Membership *is* the authorisation
model: a session carries the workspace it was created against, and
`IdentityByTokenHash` joins `workspace_members`, so a request from a
non-member resolves to no identity. Do not add a role "for later". Add it when
invites land, which is when a workspace can first hold someone who should not
be able to read or rotate its provider credentials or mint gateway keys — the
one boundary in this product that a role genuinely earns.

**Email verification is required to use the service, and mail is therefore
load-bearing.** `auth.RequireVerifiedEmail` gates the `/api` route group that
feature routes mount inside, so a route added there inherits the gate the same
way it inherits the per-user rate limit. Four endpoints are deliberately
exempt — `/me`, `/auth/logout`, `/auth/logout-all` and
`/auth/change-password` — because someone who cannot yet use the service must
still be able to see why, sign out everywhere and secure the account. Login
still succeeds and still issues a session for an unverified account: the
dashboard needs one to show the "confirm your address" state and to call
resend-verification. Because an unconfirmed address now blocks the whole
product, `SMTP_URL` is required in production, along with `MAIL_FROM` and an
https `PUBLIC_BASE_URL` — without a relay the only mailer writes the reset
link to the log, and without a correct base URL it mails links pointing at the
recipient's own machine.

Library choices are provisional (`pgx`, `go-redis`, the OpenTelemetry Go SDK, `golang-migrate` or `goose`, `golang.org/x/crypto`) — prefer the standard library wherever it is enough.

## Agent skills

### Issue tracker

Issues and specs are local markdown under `features-specs/<feature>/`; this repo has no git remote. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical labels, unchanged, applied as the `Status:` line in an issue file. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the root, neither created yet. See `docs/agents/domain.md`.
