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
| [09](issues/09-anthropic-adapter.md) | provider/anthropic adapter | `ready-for-agent` | 07 |
| [10](issues/10-v1-rate-limiting.md) | Rate limiting on /v1: reserve, then reconcile | `ready-for-agent` | 06 |
| [11](issues/11-router-fallback.md) | internal/router: fallback, skip rules, circuit breaker | `ready-for-agent` | 07, 10 |
| [12](issues/12-usage-and-metrics.md) | Usage events, rollups and metrics (Gate A: free launch) | `ready-for-agent` | 11 |
| [13](issues/13-free-first-and-cache.md) | Free-first routing and exact cache | `ready-for-agent` | 12 |
| [14](issues/14-phase-2.md) | Phase 2 (Gate B: go live) | `needs-triage` | 13 |
| [15](issues/15-delete-merged-branches.md) | Delete merged branches | `resolved` | 01 |
| [16](issues/16-smtp-end-to-end.md) | Check real mail delivery before a production deploy | `ready-for-human` | - |
| [17](issues/17-sdk-compat-check.md) | Compatibility check: the official OpenAI SDK and one IDE client | `ready-for-agent` | 06 |
| [18](issues/18-load-test.md) | Load test against the mock: under 10ms p50 added latency | `ready-for-agent` | - |
| [19](issues/19-provider-api-keys.md) | Create provider API keys on free tiers | `ready-for-human` | 03 |
| [20](issues/20-windows-scan-exclusion.md) | Optional: stop the first-run stall on new Go binaries | `ready-for-human` | - |

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

## Fog

- Whether /v1 rate limiting fails open or closed when Redis is down (ticket 10).
