# Roadmap: Phase 2, from free tiers to Gate B

The ordered list of Phase 2 work, split out of roadmap ticket 14 on
2026-11-18. Phase 1 (build order steps 1 to 7) is
[the first roadmap](../roadmap/map.md); its open tickets 16 (real mail) and 24
(the reset-password page) block go-live here too.

Phase 2 per the spec: the semantic cache, context handoff, the move to one
VPS, Prometheus and Grafana there, and custom endpoints behind an SSRF guard.
Then Gate B: go live. The control-plane additions the spec also places in
Phase 2 (budgets, logging and retention, members, key jobs) come after
Gate B and do not block it.

`SEMANTIC_CACHE` and `CONTEXT_HANDOFF` already exist in `config` and default
off. Phase 2 code stays behind them, out of Phase 1 paths.

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
`task` builds something. `R16` and `R24` name tickets in the first roadmap.

## Tickets

| # | Work | Type | Status | Blocked by |
| --- | --- | --- | --- | --- |
| **Semantic cache** | | | | |
| [01](issues/01-vector-search-on-redis.md) | Vector search on Redis: which Redis, which index shape | research | `ready-for-agent` | - |
| [02](issues/02-embedding-providers.md) | Embedding providers: who pays, which models, which dimensions | research | `ready-for-agent` | - |
| [03](issues/03-embedding-adapter.md) | Embedding adapter | task | `ready-for-agent` | 02 |
| [04](issues/04-workspace-settings.md) | Workspace settings API | task | `ready-for-agent` | - |
| [05](issues/05-semantic-cache.md) | Semantic cache in the pipeline | task | `ready-for-agent` | 01, 03, 04 |
| **Context handoff** | | | | |
| [06](issues/06-context-handoff-design.md) | Context handoff: what it is | grilling | `needs-triage` | - |
| [07](issues/07-context-handoff.md) | Context handoff | task | `needs-triage` | 06 |
| **Custom endpoints** | | | | |
| [08](issues/08-ssrf-guard.md) | SSRF-guarded HTTP client | task | `ready-for-agent` | - |
| [09](issues/09-custom-endpoints.md) | Custom OpenAI-compatible endpoints | task | `ready-for-agent` | 08 |
| **VPS and observability** | | | | |
| [10](issues/10-vps-stack.md) | The VPS stack: compose, Caddy, backups, deploy | task | `ready-for-agent` | 01 |
| [11](issues/11-provision-vps.md) | Provision the VPS, DNS and secrets | task | `ready-for-human` | 10 |
| [12](issues/12-prometheus-grafana.md) | Prometheus, Grafana and alerts on the VPS | task | `ready-for-agent` | 10 |
| [13](issues/13-cutover.md) | Cut over from Render, Neon and Upstash | task | `ready-for-human` | 11, 12 |
| **Gate B** | | | | |
| [14](issues/14-gate-b.md) | Gate B: go live | task | `ready-for-human` | 05, 07, 09, 13, R16, R24 |
| **After Gate B** | | | | |
| [15](issues/15-master-key-rotation.md) | Master key rotation job | task | `ready-for-agent` | - |
| [16](issues/16-key-health-check.md) | Periodic provider key health check | task | `ready-for-agent` | - |
| [17](issues/17-prompt-logging.md) | Opt-in prompt logging, a logs viewer and retention | task | `ready-for-agent` | 04 |
| [18](issues/18-budgets.md) | Budgets | grilling | `needs-triage` | 04 |
| [19](issues/19-members-and-invites.md) | Workspace members, invites and the role column | grilling | `needs-triage` | - |

01, 02, 04 and 08 can run in parallel, and so can 15 and 16 at any time.
07 depends on what 06 decides, so its scope is not settled yet.

## Decisions so far

- Phase 2 gets its own map rather than more rows in the first one, because
  Gate A's map is nearly closed and Phase 2 has its own gate.
- Embeddings are paid for by the workspace, through one of its own
  credentials, not by a platform key: bring-your-own-key means the platform
  pays no token costs, and an embedding is a token cost. Ticket 02 checks it
  can be done on free tiers.

## Fog

- The Anthropic-native endpoint: the spec says "later", after Phase 2.
- The dashboard frontend is out of the backend spec's scope. Ticket R24
  serves the reset-password page from the gateway until it exists.
- `GET /v1/models/{id}` (from R23), when a client is found that needs it.
- `ratelimit.Take` could return what it actually spent, so `gateway.reserve`
  stops re-deriving the burst clamp (from R23's review).
