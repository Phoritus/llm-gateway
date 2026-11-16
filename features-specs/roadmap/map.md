# Roadmap: from the mock data plane to Gate A

The ordered list of the work that is left, kept so a fresh session can pick up
without the chat that produced it. Read this first, then the ticket you take.

## How to take a ticket

0. Bring the branch up to date: `git fetch origin --prune`, then
   `git pull --ff-only` (if the current branch's PR has merged, switch to
   `main` first). Already up to date: carry on. Never force or stash to make it
   fit; if local changes or a diverged history are in the way, stop and say so.
   Delete any branch whose PR has merged, locally (`git branch -d`) and on
   origin (`git push origin --delete <branch>`).
1. Pick the lowest-numbered `ready-for-agent` ticket whose `Blocked by` tickets
   are all resolved, and set its `Status: claimed` before starting.
2. Work it the way step 2 was worked: `how` on the code it touches,
   `architect` for anything with an interface decision, `tdd`, then `deslop`,
   then the stage 4 checks (`blast-radius`, `interrogate`). `make check` must
   pass.
3. Make code and design decisions yourself. Ask the user only for what they
   must do by hand (the `ready-for-human` tickets) and for the go-ahead to
   commit, push and open a PR.
4. When it is done, set `Status: resolved`, add a line under Decisions so far,
   and update the state table in CLAUDE.md.

## Tickets

| # | Work | Status | Blocked by |
| --- | --- | --- | --- |
| [01](issues/01-merge-pr-4.md) | Merge PR #4 (step 2: mock provider through /v1) | `resolved` | - |
| [02](issues/02-crypto-seal-open.md) | internal/crypto: AES-256-GCM seal and open | `resolved` | 01 |
| [03](issues/03-provider-credentials.md) | Provider credentials: table, store, /api/credentials | `resolved` | 02 |
| [04](issues/04-gateway-keys.md) | Gateway keys: table, minting, /api/gateway-keys, in-memory cache | `resolved` | 01 |
| [05](issues/05-routes.md) | Routes: tables and /api/routes CRUD | `resolved` | 03 |
| [06](issues/06-v1-auth-and-routing.md) | /v1: authenticate gateway keys, resolve model to route, mount in production | `resolved` | 04, 05 |
| [07](issues/07-openai-adapter.md) | provider/openai: OpenAI-compatible adapter with a base URL | `resolved` | 06 |
| [08](issues/08-gemini-adapter.md) | Gemini, through the OpenAI-compatible adapter | `resolved` | 07 |
| [09](issues/09-anthropic-adapter.md) | provider/anthropic adapter | `resolved` | 07 |
| [10](issues/10-v1-rate-limiting.md) | Rate limiting on /v1: reserve, then reconcile | `resolved` | 06 |
| [11](issues/11-router-fallback.md) | internal/router: fallback, skip rules, circuit breaker | `resolved` | 07, 10 |
| [12](issues/12-usage-and-metrics.md) | Usage events, rollups and metrics (Gate A: free launch) | `resolved` | 11 |
| [13](issues/13-free-first-and-cache.md) | Free-first routing and exact cache | `resolved` | 12 |
| [14](issues/14-phase-2.md) | Phase 2 (Gate B: go live) | `needs-triage` | 13 |
| [15](issues/15-delete-merged-branches.md) | Delete merged branches | `resolved` | 01 |
| [16](issues/16-smtp-end-to-end.md) | Check real mail delivery before a production deploy | `ready-for-human` | - |
| [17](issues/17-sdk-compat-check.md) | Compatibility check: the official OpenAI SDK and one IDE client | `resolved` | 06 |
| [18](issues/18-load-test.md) | Load test against the mock: under 10ms p50 added latency | `resolved` | - |
| [19](issues/19-provider-api-keys.md) | Create provider API keys on free tiers | `ready-for-human` | 03 |
| [20](issues/20-windows-scan-exclusion.md) | Optional: stop the first-run stall on new Go binaries | `ready-for-human` | - |
| [21](issues/21-per-route-first-byte-timeout.md) | Per-route first-byte timeout | `resolved` | 11 |
| [22](issues/22-account-wide-free-limits.md) | Free-tier limits counted per account (OpenRouter) | `resolved` | 13 |
| [23](issues/23-v1-models.md) | `GET /v1/models`: list the workspace's route aliases | `needs-triage` | - |

Tickets 02 and 04 can run in parallel, and 18 can run at any time.

## Decisions so far

- Step 2 (mock through /v1): a one-method `Provider` (`ChatStream` returning a
  pull `Stream`); the commit point is the first chunk; the request deadline is
  on the socket as well as the context; `/v1` is mounted only outside
  production until ticket 06. See
  [design.md](../mock-provider-v1/design.md) and PR #4.
- Ticket 02 (crypto): a `Keyring` built from the current master key plus any
  older ones; `Seal(plaintext, aad)` returns `Sealed{KeyID, Ciphertext}`, two
  columns so rotation can query `key_id <> current`. The key id is derived
  from the key (truncated HMAC), so rotation needs no new config. Callers pass
  the row's context (workspace and credential id) as `aad`, so a ciphertext
  copied to another tenant's row does not open. The key id is not in the AAD:
  it already selects the key, and a rewritten id fails GCM on its own.
- Ticket 03 (credentials): `store.Credentials` owns sealing, so the AAD
  (workspace id, row id, vendor) is private to it and no caller builds one.
  The provider is a fixed vendor set, not a stored base URL, because a
  user-supplied URL is the Phase 2 SSRF surface; ticket 07 attaches base URLs
  in code. A workspace holds at most 50 credentials, enforced by locking the
  workspace row, since a bare count-then-insert overshot under concurrency.
- Ticket 04 (gateway keys): a key is `gwk_` plus 32 random bytes in base64url;
  only its SHA-256 and a 12-character display prefix are stored. `auth.GatewayKeys`
  authenticates through a 1-second in-process cache: the revoking instance
  evicts at once, other instances stop accepting within the TTL, so no
  cross-instance invalidation channel is needed. Revoke is a soft delete
  (`revoked_at`) so usage rows can still name the key; at most 50 unrevoked
  keys per workspace. `last_used_at` is batched in memory and written once a
  minute per instance, and once more on shutdown.
- Ticket 05 (routes): `route_targets` carries `workspace_id` with composite
  foreign keys to `routes (id, workspace_id)` and `provider_credentials (id,
  workspace_id)`, so a target naming another tenant's credential is refused by
  the database, not by a handler check. Deleting a credential a route still
  names is refused (409 `credential_in_use`) rather than cascaded, so a chain
  never shrinks without its owner seeing why. PUT replaces the route and its
  whole chain; an omitted `max_hops` means the default (3), bounded 1 to 10.
  Aliases are case-sensitive, at most 64 characters of `[A-Za-z0-9._:/-]`; at
  most 10 targets per route (no repeats) and 100 routes per workspace.

- Ticket 06 (/v1 auth and routing): the handler authenticates before reading
  the body, then resolves `model` through `store.Credentials.ResolveRoute`,
  which opens the whole chain in one query so the router (ticket 11) gets
  every hop without another round trip. `gateway.Deps` holds an adapter per
  `provider.Vendor`; a vendor with no adapter answers 502
  `provider_unavailable`, which is every real vendor until ticket 07. `mock`
  is a vendor (migration 00005): the credentials API refuses it in
  production and production registers no mock adapter. Email verification is
  the workspace owner's, joined into the key lookup and cached with the key
  for its 1 second. No IP limit or negative cache in front of key lookup yet:
  a miss is one unique-index lookup, and ticket 10 owns /v1 limiting.

- Ticket 07 (OpenAI-compatible adapter): one `openai.Provider` per vendor in
  `openai.Endpoints` (OpenAI, Groq, Cerebras, OpenRouter, Mistral), each with
  its own `http.Client` (5s dial and TLS, no redirects followed) and a 30s
  first-byte timer that stops at the first data event, so a stalled upstream
  is `ErrTimeout` while failover is still legal. SSE framing lives in
  `provider/sse` for the Gemini and Anthropic adapters to share. Two new
  kinds: `ErrInvalidRequest` (upstream 400/422, answered 400) and
  `ErrModelUnavailable` (403/404, the router's "skip that model on that
  credential"). `ChatRequest.Extra` carries every top-level field it does not
  model through to the upstream. `Provider` gained `CheckKey`, a GET that
  generates nothing (OpenRouter's `/key`, since its `/models` is public),
  behind `POST /api/credentials/{id}/test`, which answers 200 with `ok` and a
  fixed `code`, never the upstream's words.

- Ticket 08 (Gemini): no native adapter. Google serves Gemini over an
  OpenAI-compatible endpoint (`generativelanguage.googleapis.com/v1beta/openai`,
  Bearer key, streaming, tools, images, `stream_options`, `GET /models`), so
  Gemini is one more entry in `openai.Endpoints` and `provider/gemini` is
  deleted. Only its errors differ, and `classify` reads them: a root-level
  array (on chat completions; `/models` answers a bare object), a bad key as a
  400 `INVALID_ARGUMENT` recognised only by its message (`ErrAuth`; checked
  live with a fake key), and one 429
  `RESOURCE_EXHAUSTED` for both limits, split by the QuotaFailure's `quotaId`
  (`PerDay` is `ErrQuota`, anything else `ErrRateLimited`), with RetryInfo's
  `retryDelay` as the retry hint when no header gives one. Google calls the
  compatibility layer beta. If it drops a feature a route needs, the native
  `generateContent` adapter is the fallback plan, not a default.

- Ticket 09 (Anthropic adapter): raw `net/http` like the other adapters, not
  the Go SDK, whose own retries would compete with the router's failover. The
  HTTP plumbing (transport, first-byte timer, `callError`) is copied from
  `provider/openai` rather than shared; extract it when a third adapter needs
  it. It sends `anthropic-version: 2026-10-21` and never forwards
  `temperature` or `top_p`: every current Claude model refuses non-default
  values and that version refuses the fields outright, while OpenAI clients
  send them by default. `max_tokens` defaults to 32000 when the client sends
  none. `response_format` `json_schema` becomes `output_config.format`;
  `json_object`, `n` > 1 and unknown content parts are refused before any call.
  Thinking blocks are not forwarded to the client. A 529 overloaded is
  `ErrUpstream`, not a rate limit: it is not the account's limit. Only the
  bad-key answer was checked live (401 `authentication_error`); a real key
  (ticket 19) is needed to check the version header and a full stream.
- Ticket 10 (/v1 rate limiting): a request spends four buckets (requests and
  estimated tokens, per key and per workspace) through `ratelimit.Take`, one
  Lua call that admits all or nothing, so a refusal spends none and admission
  is one round trip. The estimate is the body's bytes over four, prompt only;
  a cost above a bucket's burst is clamped so it can be admitted. After the
  response, `Settle` corrects the token buckets to the reported usage in a
  goroutine: a refund when the upstream failed before its first chunk, the
  reservation kept when a generation broke without reporting. Debt is floored
  at minus the burst. Usage is always asked of the upstream and hidden from a
  client that did not ask. Redis down: /v1 fails open to `ratelimit.Local`
  and skips Redis for 5s after an error, because these limits guard the
  workspace's own provider budget; the control plane still fails closed. The
  limit runs before route resolution, which is a Postgres query. In-flight
  requests per workspace are capped per instance. Failed authentications cost
  one per IP (30 a minute); once that refuses, the IP is blocked in process
  until the bucket's Retry-After, so a flood of unknown keys stops reaching
  Postgres. A blocked IP is refused even with a valid key, so one bad client
  behind a shared NAT can cost its neighbours a couple of seconds at a time.
  `take.lua` touches several keys at once, fine on one Redis node and on
  Upstash, but a Redis Cluster would need hash tags in the key format.

- Ticket 11 (router): the attempt loop is `router.Run`, handed an `Attempt`
  that opens a stream and reads its first chunk, so the router owns failover
  and the gateway owns adapters. Skip rules follow the spec's table in
  `rules.go`. `ErrQuota` covers both no-credit and a daily quota, so it skips
  the credential and model (the narrower subject) until the upstream's hint,
  else for an hour. A rate limit skips the credential for 90s, doubling on
  repeats to 15m; an upstream hint above that is honoured, capped at 24h. 5xx
  and timeouts count toward a per-vendor breaker (5 in 60s opens it for 30s,
  across workspaces) and a per-credential-and-model bench (3 in 15m, 10m).
  A success clears the vendor's count, so ordinary error rates or one
  workspace's slow model cannot open the breaker for everyone. Context too
  long skips later targets with the same model in that request only; an
  invalid request still fails over, since adapters accept different things.
  Exhaustion is rate limited if any target was refused or skipped for a limit,
  with Retry-After until the earliest such skip ends rather than the
  upstream's hint, which the skip already outlasts; otherwise the failure
  every attempt shared, otherwise 502. Skip state is fixed-window counters and
  TTL'd keys in Redis, read in one MGET with a 1s in-process copy, written
  behind the request; Redis errors fall back to in-process state. Keys escape
  `%` and `:` so a model such as `meta/llama:free` cannot collide. The
  per-route first-byte timeout from ticket 08's comment became ticket 21.

- Ticket 12 (usage and metrics): the write and read sides of usage are
  separate inside `usage` only, not an app-wide CQRS. One event per request
  that resolved a route; requests refused before it (auth, validation, rate
  limit, concurrency, unknown model) get metrics only, so a flood cannot
  flood the table. Events carry the route alias, vendor and model as text, so
  history survives a deleted route or credential. Each event
  carries a UUID minted when it is recorded, and a batch is one statement: it
  inserts the raw rows, skipping an id already stored, and adds only the rows
  it inserted to `usage_daily` (workspace, UTC day, alias, vendor, model). A
  failed batch therefore counts nothing and a batch retried after a lost
  acknowledgement counts once. A batch that fails twice is written one event
  at a time, so an event the database refuses (a deleted workspace) costs only
  itself; token and latency columns are `bigint` so an upstream's count cannot
  be that event. Lost events are counted in `usage.events.dropped` by reason:
  `overflow` (full queue), `write_failed`, or `shutdown` (recorded after
  `Close`, or abandoned at its deadline). `errors` in the rollup is every
  outcome except `ok` and `client_gone`; a stream that fails after its first
  chunk is status 200 with outcome `stream_error`. Metric labels are vendor
  and outcome only, never alias, workspace or key; breaker state is
  `router.breaker.opened`, since a live gauge cannot be read reliably from
  per-instance copies. It counts crossings: the one failure whose count equals
  the threshold, not the failures after it that extend the breaker. A client
  hanging up mid-call is not counted in `router.upstream.errors`; the
  request's deadline running out is. Shutdown is one budget, `SHUTDOWN_TIMEOUT`
  (default 25s, at least 5s), from SIGTERM to exit: requests drain for all but
  the last 4s and are then cut, the usage flush runs to 1s before the end and
  the telemetry flush to the end. It must stay under the platform's kill
  delay. Render's is 30s by default and `maxShutdownDelaySeconds` raises it to
  at most 300s, so streams as long as `V1_REQUEST_TIMEOUT` survive a deploy
  only if both are raised together (300 and `SHUTDOWN_TIMEOUT=290s`); with the
  defaults a deploy cuts them.

- Ticket 13 (free-first and exact cache): see
  [design.md](../free-first-and-cache/design.md). A route's `cache` mode is
  `auto` (temperature 0 only, the default), `always` or `off`; there is no
  workspace-wide switch until a workspace settings API exists. The key hashes
  the route id, its `updated_at` and the request with `model`, `stream`,
  `stream_options` and `user` removed and its JSON re-encoded, so editing a
  chain invalidates its answers with no purge step (a PUT that changes nothing
  does too). Tool requests and incomplete or tool-calling answers are never
  stored; null tool fields count as absent, since SDKs echo `tool_calls: null`
  every turn. A hit is replayed through the same send path as a live stream,
  refunds the token reservation, records zero tokens with `cached = true` and
  counts in `usage_daily.cache_hits`; its metrics carry vendor `cache`. The
  lookup gives up after 250ms and any cache error is a miss. Free-first only
  reorders: targets with a `free_quota_catalog` entry and room in their
  per-credential-and-model buckets go first, the rest keep the user's order
  behind, because the counts are this gateway's view and the router's skip
  rules handle a real 429. Every answered request spends its target's free
  buckets, whatever the route. The catalog seed was checked with free keys
  for Groq, Gemini and OpenRouter and holds only reported limits (null
  otherwise); Cerebras is unchecked and Mistral is now paid-only, so neither
  has rows. OpenRouter's 50 free requests a day are account-wide, so several
  `:free` models on one key were over-counted as free until ticket 22.

- Ticket 17 (client compatibility): `make compat` checks the OpenAI Python
  SDK and the Continue CLI (the Continue IDE extensions' core) against a
  running development gateway and the mock, creating a throwaway account each
  run and confirming its address directly in the development database. Both
  clients worked with no gateway change: answers, streams, and every error
  surfaced as the client's own error with our status, code and message,
  including a failure after the first chunk. `GET /v1/models` is missing and
  went unused; it is ticket 23, for triage.

- Ticket 18 (load test): `make loadtest` runs vegeta against `mock/echo` on a
  gateway it starts itself with the `/v1` limits raised, so it measures the
  gateway rather than its 429s. On the dev machine p50 added latency is about
  1.6ms at 100/s and 2.6 to 3.2ms at 500/s, against a 10ms target; peak
  memory is about 150 MB at either rate, against Render's 512 MB. No gateway
  change was needed.

- Ticket 21 (first-byte timeout): per target, not per route
  (`first_byte_timeout_seconds`, 1 to 300, null is 30s), because only the
  slow model in a chain should wait longer. The timer belongs to the
  gateway's `attempt` now, not to each adapter. The router's 45s budget
  grows by the extra wait of each target it attempted, so a long-thinking
  first target does not cost the rest of the chain its time.

- Ticket 22 (account-wide free limits): one boolean on the catalog row,
  `per_account`, not a per-limit scope, because the only account-wide limit
  known (OpenRouter's 50 free requests a day) is the only limit its rows
  carry. A per-account row's buckets drop the model from their id, so every
  `:free` model on one key draws on one allowance. Split it per limit when a
  vendor turns out to mix the two on one row.

## Fog

- None open.
