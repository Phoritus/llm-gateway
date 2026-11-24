# Roadmap: Phase 2, from free tiers to Gate B

The ordered list of Phase 2 work, split out of roadmap ticket 14 on
2026-11-18. Phase 1 (build order steps 1 to 7) is
[the first roadmap](../roadmap/map.md); its open ticket 16 (real mail) blocks
go-live here too.

Phase 2 per the spec: the semantic cache, context handoff, the move to one
VPS, Prometheus and Grafana there, and custom endpoints behind an SSRF guard.
Then Gate B: go live. The control-plane additions the spec also places in
Phase 2 (budgets, logging and retention, members, key jobs) come after
Gate B and do not block it.

`SEMANTIC_CACHE` and `CONTEXT_HANDOFF` already exist in `config` and default
off. Phase 2 code stays behind them, out of Phase 1 paths.

## Where it stands (2026-11-23)

Production runs on one Azure VM: the gateway and Caddy under compose
(`deploy/`), behind Cloudflare, shipped with `make deploy`. Postgres is
Azure's managed Postgres, Redis stays on Upstash, and metrics and traces
stay on Grafana Cloud. There is no Prometheus and no self-hosted Postgres to
run. The VM's own `redis:8-alpine` holds only the semantic cache's vectors,
because Upstash has no vector sets. That shrinks tickets 10, 12 and 13.

**Done:** 01 to 12, 15 and 16.

**Next, for an agent, in this order:**

1. **17 and 18**, which need nothing from the owner: opt-in prompt logging,
   budgets.

**Next, for the owner:**

- **R16:** a real SMTP relay in `deploy/.env.prod`, and one signup whose mail
  arrives.
- **R19:** free-tier provider keys, then `make live`.
- **05:** turn it on: `SEMANTIC_CACHE=true` and `VECTOR_REDIS_URL=redis://redis:6379` in `deploy/.env.prod`, then calibrate the 0.95 thresholds on real traffic.
- **12:** in Grafana Cloud, create a service account token (Editor, plus
  alert rule write), put `GRAFANA_URL` and `GRAFANA_TOKEN` in `.env.dev`,
  run `make grafana`, and set your email on the default contact point.
- **10:** check the VM has 2 GiB of memory (the stack takes up to 832 MiB
  and the image builds on the VM), then `make deploy`. The first deploy
  moves to two gateways and may drop one request as Caddy reloads.
- **07:** turn it on: `CONTEXT_HANDOFF=true` in `deploy/.env.prod`. Every route then reorders by window; a route set to `trim` also fits long conversations.
- **13:** move the data from Neon to Azure Postgres, if Neon still holds any.

Then **14, Gate B**. Tickets 15 to 18 come after it.

## How to take a ticket

Exactly as in [the first roadmap](../roadmap/map.md#how-to-take-a-ticket):
bring the branch up to date, take the lowest-numbered `ready-for-agent`
ticket whose `Blocked by` tickets are all resolved, set `Status: claimed`,
work it (`how`, `architect` for an interface decision, `tdd`, `deslop`, then
`blast-radius` and `interrogate`; `make check` must pass), then set
`Status: resolved` with an `## Answer`, add a line under Decisions so far, and
update the state table in CLAUDE.md.

A `Type:` line says what kind of ticket it is: `research` answers a question
and writes the answer down (a prototype is fine, merged code is not the
point), `grilling` needs the user's decisions before anything is built,
`task` builds something. `R16` names a ticket in the first roadmap.

## Tickets

| # | Work | Type | Status | Blocked by |
| --- | --- | --- | --- | --- |
| **Semantic cache** | | | | |
| [01](issues/01-vector-search-on-redis.md) | Vector search on Redis: which Redis, which index shape | research | `resolved` | - |
| [02](issues/02-embedding-providers.md) | Embedding providers: who pays, which models, which dimensions | research | `resolved` | - |
| [03](issues/03-embedding-adapter.md) | Embedding adapter | task | `resolved` | 02 |
| [04](issues/04-workspace-settings.md) | Workspace settings API | task | `resolved` | - |
| [05](issues/05-semantic-cache.md) | Semantic cache in the pipeline, vectors in the VM's Redis | task | `resolved` | 01, 03, 04 |
| **Context handoff** | | | | |
| [06](issues/06-context-handoff-design.md) | Context handoff: what it is | grilling | `resolved` | - |
| [07](issues/07-context-handoff.md) | Context handoff | task | `resolved` | 06 |
| **Custom endpoints** | | | | |
| [08](issues/08-ssrf-guard.md) | SSRF-guarded HTTP client | task | `resolved` | - |
| [09](issues/09-custom-endpoints.md) | Custom OpenAI-compatible endpoints | task | `resolved` | 08 |
| **The VM and observability** | | | | |
| [10](issues/10-vps-stack.md) | Finish the VM stack: memory limits, a deploy that does not cut requests | task | `resolved` | - |
| [11](issues/11-provision-vps.md) | Provision the VM, DNS and secrets | task | `resolved` | - |
| [12](issues/12-prometheus-grafana.md) | Dashboards and alerts on Grafana Cloud | task | `resolved` | - |
| [13](issues/13-cutover.md) | Move Postgres from Neon to Azure | task | `ready-for-human` | - |
| **Gate B** | | | | |
| [14](issues/14-gate-b.md) | Gate B: go live | task | `ready-for-human` | 05, 07, 09, 10, 12, 13, R16 |
| **After Gate B** | | | | |
| [15](issues/15-master-key-rotation.md) | Master key rotation job | task | `resolved` | - |
| [16](issues/16-key-health-check.md) | Periodic provider key health check | task | `resolved` | - |
| [17](issues/17-prompt-logging.md) | Opt-in prompt logging, a logs viewer and retention | task | `ready-for-agent` | 04 |
| [18](issues/18-budgets.md) | Budgets, in money, per workspace and per key | task | `ready-for-agent` | 04 |
| [19](issues/19-members-and-invites.md) | ~~Workspace members, invites and the role column~~ (cut) | grilling | `wontfix` | - |

01, 02, 04 and 08 can run in parallel, and so can 15 and 16 at any time.

## Decisions so far

- Phase 2 gets its own map rather than more rows in the first one, because
  Gate A's map is nearly closed and Phase 2 has its own gate.
- Embeddings are paid for by the workspace, through one of its own
  credentials, not by a platform key: bring-your-own-key means the platform
  pays no token costs, and an embedding is a token cost. Ticket 02 checks it
  can be done on free tiers.

- Ticket 01 (vector search): Redis 8 Open Source under AGPLv3, unpatched.
  Vector sets, one key per workspace and model (`vec:{workspace}:{family}:{dim}`),
  so tenant isolation is the key, not a filter. Q8 at about 1.8 KB an entry.
  A count cap evicts the oldest in one Lua script with `vecage:` and
  `vecans:`, and the cosine threshold `c` is `EPSILON (1 - c) / 2`. See
  [research.md](vector-search/research.md).

- Ticket 02 (embedding providers): `gemini-embedding-2` at 1024 dimensions
  through the workspace's Gemini credential is the default family; OpenRouter's
  `google/gemini-embedding-2` is its in-family failover, and `bge-m3` the
  second family. Every endpoint speaks OpenAI's `POST /embeddings` with
  `dimensions`, so ticket 03 needs one client keyed by base URL. No credential
  for a family means no semantic cache; no local model on the VPS. Thresholds
  are per family, since nothing reached 0.95. See
  [research.md](embeddings/research.md).

- Ticket 03 (embedding adapter): `provider.Embedder` beside `Provider`, not a
  method on it, because Anthropic cannot implement it. Families are a table in
  `cache`, each mapping a vendor to the model name it serves the family under.
  Failover is ordered credentials under one deadline for the whole call, with
  no skip state or breaker: a failure is a miss, so the router's machinery
  would buy nothing. Vectors are normalised here, never trusted.

- Ticket 04 (workspace settings): `GET`/`PUT /api/workspace/settings`, a
  flat document replaced whole (`cache_enabled` today). Settings ride on
  `ResolveRoute`'s query, so a change applies to the next request, and
  `cache.RequestCacheable` takes the resolved route so it still decides alone.
  Stored fields default at their zero value.

- Ticket 06 (context handoff), decided: stateless, inside the router's
  pre-first-byte loop, with no `sess:` key and no summarising call. Every
  route reorders targets by known window by default; a route set to `trim`
  also fits the conversation (system, tools and final turn kept, the first
  user message not pinned; old tool results cleared; whole middle units
  dropped behind a marker; tool pairs never split), announced in
  `X-Context-Handoff`. Windows are learned from errors, with a seeded
  catalogue as the guess. Exhaustion becomes 400 `context_length_exceeded`.
  See [research.md](context-handoff/research.md) and ticket 06.

- Hosting, the owner's decision (2026-11-23): one Azure VM for the gateway
  and Caddy, Azure's managed Postgres in place of Neon, Upstash kept for
  Redis, Grafana Cloud kept for metrics, traces and alerts. Nothing moves to
  a self-hosted Prometheus or Postgres. Backups are Azure's own. The VM's
  `redis:8-alpine` stays, for the semantic cache's vectors only (ticket 05).
- Ticket 05 (semantic cache): a hit needs the same context hash, so only the
  last user message may vary; embed through the route's own targets, no extra
  query; cap 1000 oldest-first per workspace and family; 7-day idle expiry.
  See [the design](../semantic-cache/design.md).
- Ticket 18 (budgets), the owner's decision: money, not tokens, as other
  gateways do; per workspace and per gateway key; block or warn chosen per
  budget, block by default. A price table beside `free_quota_catalog` is
  part of the ticket.
- Ticket 19 (members and invites): cut by the owner to keep to the core.
  Workspaces stay single-member and there are no roles.

- Ticket 07 (context handoff): the router owns it, because the windows it
  learns are shared state read in the same MGET as the skips, and it is the
  only place that knows which call is a retry. `internal/handoff` is the pure
  half (`Fit`, `Budget`, the catalogue). The catalogue is a file embedded
  at build time by `make windows`, not a table: learned windows win, and
  the upstreams' windows change far less often than their free quotas.
  Fitting never refuses a target on its own: one whose window cannot hold
  the pinned messages is sent the request as it came, since the window may
  be the catalogue's guess, and its error decides.

- Ticket 16 (key health): a due time per credential, claimed with
  `SKIP LOCKED` and pushed a day ahead in the same statement, rather than
  checking every credential at a fixed hour. Instances share the work with
  no lock or leader, and the load spreads by when each key was added. The
  checker never touches the router's skip state.

## Fog

- The Anthropic-native endpoint: the spec says "later", after Phase 2.
- The dashboard frontend, the reset-password page included, lives in its
  own repo (R24 is wontfix here). Until it serves `/reset-password`, the
  mailed reset link answers 404.
- `GET /v1/models/{id}` (from R23), when a client is found that needs it.
- A window learned from an error is only ever lowered or replaced by another
  error, never raised by a success, so a small one learned on a free tier
  keeps trimming for its 7 days (from ticket 07's review).
- `ratelimit.Take` could return what it actually spent, so `gateway.reserve`
  stops re-deriving the burst clamp (from R23's review).
