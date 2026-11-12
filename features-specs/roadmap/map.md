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
| [03](issues/03-provider-credentials.md) | Provider credentials: table, store, /api/credentials | `claimed` | 02 |
| [04](issues/04-gateway-keys.md) | Gateway keys: table, minting, /api/gateway-keys, in-memory cache | `ready-for-agent` | 01 |
| [05](issues/05-routes.md) | Routes: tables and /api/routes CRUD | `ready-for-agent` | 03 |
| [06](issues/06-v1-auth-and-routing.md) | /v1: authenticate gateway keys, resolve model to route, mount in production | `ready-for-agent` | 04, 05 |
| [07](issues/07-openai-adapter.md) | provider/openai: OpenAI-compatible adapter with a base URL | `ready-for-agent` | 06 |
| [08](issues/08-gemini-adapter.md) | provider/gemini adapter | `ready-for-agent` | 07 |
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

## Fog

- How `mock` is exposed once routes exist (ticket 06).
- Whether /v1 rate limiting fails open or closed when Redis is down (ticket 10).
- How a revoked gateway key is evicted on every instance (ticket 04).
