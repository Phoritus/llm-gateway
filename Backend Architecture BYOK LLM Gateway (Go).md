# Backend Architecture: BYOK LLM Gateway (Go)

Nov 3, 2026 · @Poo

## Goals and scope

The backend is one Go service with two jobs: a fast gateway path for model requests, and a management API for the dashboard. It must add little latency, cost almost nothing to host in Phase 1, and keep every user's keys, limits and cache separate.

**Decisions inherited from the proposal**

- Bring your own key: users add provider keys, so the platform pays no token costs.
- Go, on free tiers in Phase 1 (Render, Upstash Redis, Neon Postgres) and one VPS in Phase 2.
- OpenAI-compatible `/v1/chat/completions` first, with streaming. An Anthropic-format endpoint comes later.
- Fallback chains, a circuit breaker, token-bucket limits and free-first routing. Exact-match cache in Phase 1, semantic cache in Phase 2.

**Targets (proposed; confirm by measurement)**

- Added latency under 10 ms at p50, measured against a mock provider.
- Failover to the next provider within 1 second, before the first token.
- Memory comfortably under the 512 MB of Render's free instance.

**Out of scope for this document:** the frontend, payments, team members and the Anthropic-native endpoint.

## Components and process layout

&#91;embedded content: components · 1 service, 2 stores, 1 external side\]

The whole backend ships as one Go binary in one container. Apps call the data plane with a gateway key; the dashboard calls the control plane with a session. Both planes share the same code for keys, encryption and storage. The data plane keeps hot configuration (keys, routes) in memory, loaded from Postgres, and keeps fast shared state in Redis. Splitting the planes into separate services is possible later without changing the interfaces.

## Request pipeline

Every call to `/v1/chat/completions` passes through the same ordered steps. Each step is a small Go function behind an interface, so it can be tested alone and swapped later.

1. **Authenticate.** Hash the bearer gateway key (SHA-256) and look it up in an in-memory cache backed by Postgres. Resolve key to workspace. Reject revoked or unknown keys with 401.
2. **Parse and validate.** Decode the OpenAI-format body, check size limits, and resolve the requested `model` to a route (alias) owned by that workspace.
3. **Rate limit.** Run the Redis token-bucket script per gateway key and per workspace. Reserve an estimated token cost now and reconcile after the provider replies. Reject with 429 and `Retry-After`.
4. **Cache lookup.** Exact-match hash of the normalized request (Phase 1). Semantic lookup is added in Phase 2. A hit returns immediately and skips the provider.
5. **Route and call.** The routing engine picks an ordered list of targets (provider key + model) and tries them with the fallback rules in the routing section.
6. **Stream back.** Copy provider chunks to the client as they arrive, flushing each one. Count tokens as they pass.
7. **Finish.** Reconcile the rate-limit reservation, store a cacheable response, and push a usage event to a buffered channel. A background worker writes it to Postgres in batches, so logging never delays the response.

**Rules that hold at every step**

- The request context carries a deadline and is cancelled when the client disconnects, which stops the upstream call and avoids paying for abandoned generations.
- Failover is allowed only before the first byte is sent to the client. After that, an upstream error ends the stream with an error event.
- Errors use the OpenAI error shape so SDKs and IDE clients handle them without special code.

## Provider adapters and streaming

The core speaks one internal request and response shape (OpenAI chat format). Each provider has an adapter that translates to and from it.

**Adapter interface (sketch)**

```go
type Provider interface {
    Name() string
    Chat(ctx context.Context, req *ChatRequest, key Credential) (*ChatResponse, error)
    ChatStream(ctx context.Context, req *ChatRequest, key Credential) (Stream, error)
    Embed(ctx context.Context, req *EmbedRequest, key Credential) (*EmbedResponse, error)
}
```

Adapters return typed errors (`ErrAuth`, `ErrQuota`, `ErrRateLimited`, `ErrContextTooLong`, `ErrUpstream`, `ErrTimeout`) so the router can pick a skip rule without parsing messages.

**Phase 1 adapters:** most free providers (Groq, Cerebras, OpenRouter, Mistral and others) already speak the OpenAI format, so one generic OpenAI-compatible adapter with a base URL covers many of them. Gemini is one of them, through Google's OpenAI-compatible endpoint (`/v1beta/openai`); only its error bodies differ. Anthropic needs its own adapter.

**Streaming**

- Server-Sent Events over `net/http`, with `http.Flusher` called after every chunk and buffering disabled (`X-Accel-Buffering: no`).
- The upstream response is read line by line; usage is taken from the final chunk when the provider reports it, otherwise estimated at about 4 characters per token.
- A mid-stream provider failure is sent as a final error event, then the stream closes.
- One shared `http.Client` per provider with connection reuse, a dial timeout and a time-to-first-byte timeout.

**Token counting:** no exact tokenizer in Go, so estimate before the call and trust the provider's reported usage afterwards. Estimates only need to be good enough for rate-limit reservation.

## Routing, fallback and quotas

A **route** is a user-defined alias such as `my-chat` that maps to an ordered chain of targets. Each target is a provider credential plus a model name. The routing engine turns a route into the list of targets to try.

**Selection order**

1. Drop targets that are currently skipped (see table) or whose circuit breaker is open.
2. If the route has free-first enabled, move targets with remaining free quota to the front.
3. Otherwise keep the user's order.
4. Try targets in turn until one succeeds or a limit is reached.

**Skip rules (adapted from FreeLLMAPI)**

| Upstream result | What is skipped | For how long |
| --- | --- | --- |
| 401 bad key | that credential | 5 minutes |
| 402 no credit | that credential | 24 hours |
| 403 model not allowed | that model on that credential | 24 hours |
| 429, daily quota | that credential and model | until the quota resets |
| 429, per-minute | that credential | 90 seconds, growing on repeats |
| 5xx or timeout | the whole provider | short, with the circuit breaker |
| Context too long | that model | that request only |

**Limits per request**

- Max hops: the proposal says 3. FreeLLMAPI allows 20. Keep 3 as the default and make it a per-route setting.
- Time budget: 45 seconds in total; the first retry is always allowed.
- A model that fails 3 times in 15 minutes is benched for 10 minutes.

**Where state lives:** skip flags and breaker state are Redis keys with TTLs so every instance agrees and expiry is automatic. A small in-process copy (a second or two) avoids a Redis call on each request.

**Honest exhaustion:** when every target fails, return 429 with `Retry-After` if any failure was a limit, 413 if all were context errors, otherwise 502. Never return 500 for upstream trouble. Set `X-Fallback-Attempts` and `X-Fallback-Trail` headers so users can see what happened.

**Free-first quotas:** a Postgres table lists known free limits per provider and model (requests per minute and per day, tokens per minute and per day). Redis token buckets track usage per credential against those limits. This is opt-in per route, with the privacy warning from the proposal.

## Rate limiting

Limits protect the user's own provider budget and stop one key from starving the rest. They use a token bucket in Redis, run as one Lua script so the check and the update are atomic across instances.

- **Scopes:** per gateway key, per workspace, and per provider credential (the last one enforces known free-tier limits).
- **Two units:** requests per minute and estimated tokens per minute.
- **Reserve then reconcile:** before the call, take the estimated tokens. After the response, add or refund the difference between the estimate and the provider's reported usage.
- **Refill:** `tokens = min(capacity, tokens + elapsed * rate)`; admit when `tokens >= cost`; keys expire after an hour of silence.
- **Response:** 429 with `Retry-After` computed from the refill rate, plus `X-RateLimit-Remaining` headers.

**If Redis is down:** fail open with a small in-memory per-instance limiter and raise an alert. A Redis outage should degrade protection, not take the gateway offline.

**Concurrency:** also cap in-flight streams per workspace with a semaphore, since long streams are what exhaust memory first.

## Caching and embeddings

**Exact cache (Phase 1).** Key = SHA-256 of workspace id, route, normalized messages and sampling parameters. Only cache when `temperature` is 0 or the user turns caching on for the route. Value is the full response in Redis with a TTL. Streaming requests on a hit are replayed as a stream.

**Semantic cache (Phase 2).**

- Embed the last user message, search the workspace's vector index (Redis Stack), and return the stored answer above a similarity threshold. Start high (the original project brief says above 95%) and tune with real traffic.
- Every cache is isolated per workspace, so one tenant can never receive another's answer.
- One embedding model family per cache. Store the model family and dimensions on each entry and on the index; changing model starts a new index.
- If embedding fails or times out, treat it as a cache miss and continue to the provider.
- Prefer smaller vectors: 1024 dimensions is about 4 KB per entry against about 12 KB for 3072, which matters on a 30 MB free Redis.

**Embedding adapter.** One interface with interchangeable clients: FreeLLMAPI, Cloudflare Workers AI, Gemini, or a small local model on the Phase 2 VPS. Failover stays inside the same model family so vectors remain comparable.

**Never cache:** requests with tool calls, responses flagged as errors, or workspaces that turned caching off.

## Data model

**Postgres (durable data)**

| Table | Purpose | Key columns |
| --- | --- | --- |
| `users` | Sign-in identities | id, email, password\_hash or oauth id |
| `workspaces` | Tenant boundary | id, name, owner\_id, settings |
| `workspace_members` | Who belongs (invites later) | workspace\_id, user\_id — a role column arrives with invites in Phase 2, not before |
| `gateway_keys` | Keys apps use | id, workspace\_id, hash, prefix, last\_used, revoked\_at |
| `provider_credentials` | User's provider keys | id, workspace\_id, provider, ciphertext, nonce, last4 |
| `routes` | Aliases and chains | id, workspace\_id, alias, free\_first, max\_hops |
| `route_targets` | Ordered chain entries | route\_id, position, credential\_id, model |
| `free_quota_catalog` | Known free limits | provider, model, rpm, rpd, tpm, tpd |
| `usage_events` | One row per request | workspace\_id, key\_id, route, target, tokens in/out, latency, status, cached |
| `usage_daily` | Rollup for the dashboard | workspace\_id, day, route, totals |

Every tenant table carries `workspace_id` and every query filters on it. Raw `usage_events` are pruned after a retention period; the daily rollup is kept. Prompt text is not stored unless the user opts in.

**Redis (fast, expiring data)**

| Key pattern | Holds | TTL |
| --- | --- | --- |
| `rl:{scope}:{id}` | Token bucket state | 1 hour idle |
| `skip:{credential}:{model}` | Skip flag | per skip rule |
| `cb:{provider}` | Circuit breaker state | short |
| `cache:{workspace}:{hash}` | Exact-match response | configurable |
| `vec:{workspace}` | Semantic index (Phase 2) | none, size capped |
| `sess:{workspace}:{session}` | Last model and recent messages for handoff (Phase 2) | 3 hours |

Migrations use a versioned tool (goose or golang-migrate; to be chosen). Postgres access via `pgx`.

## Control plane API

The dashboard talks to a JSON API under `/api`, separate from the `/v1` data path. It uses session cookies (HttpOnly, SameSite) rather than gateway keys, and is rate limited on its own.

| Area | Endpoints (proposed) | Notes |
| --- | --- | --- |
| Auth | `POST /api/auth/signup`, `login`, `logout`, `GET /api/me` | Password hashing with argon2id or bcrypt |
| Provider keys | `GET/POST /api/credentials`, `DELETE /api/credentials/{id}`, `POST .../test` | List shows last 4 characters only; a test call validates the key |
| Routes | `GET/POST /api/routes`, `PUT/DELETE /api/routes/{id}` | Includes ordered targets, free-first flag, max hops |
| Gateway keys | `GET/POST /api/gateway-keys`, `DELETE /api/gateway-keys/{id}` | Full key shown once at creation |
| Usage | `GET /api/usage?from=&to=&route=` | Reads the daily rollup, with a recent-requests list |
| Health | `GET /healthz`, `GET /readyz` | Liveness and readiness (Postgres and Redis reachable) |

Phase 2 adds budgets, embedding provider settings, log retention, a logs viewer and workspace members.

Input is validated on the server, errors use one JSON shape, and every handler gets the workspace id from the session, never from the request body.

## Security

- **Provider keys:** encrypted with AES-256-GCM using a master key from the environment or a secret manager, never stored in the database. A fresh nonce per value. Only the last 4 characters are ever returned to the browser; the full key never leaves the server.
- **Gateway keys:** random, with a readable prefix; stored as a SHA-256 hash and shown once. Revocation takes effect immediately by evicting the in-memory cache entry.
- **Tenant isolation:** `workspace_id` on every row, enforced in a single data-access layer, with tests that try cross-tenant reads.
- **Prompts and responses:** not logged by default. Logging is opt-in per workspace, with retention limits.
- **Logs and errors:** redact `Authorization` headers and anything that looks like a key before writing.
- **Custom endpoints (Phase 2):** block private, loopback and link-local addresses, including after redirects and DNS resolution, to prevent server-side request forgery.
- **Transport:** TLS at the edge (Render, or Caddy on the VPS); secure cookies; CORS limited to the dashboard origin on `/api`.
- **Abuse limits:** signup rate limiting and request size caps.
- **Master key rotation:** store a key id with each ciphertext so keys can be re-encrypted in a background job.

## Observability and background jobs

**Metrics (OpenTelemetry)**

- Request count and latency by route and outcome, time to first token, fallback hops, cache hit rate, rate-limit rejections, circuit breaker state, and upstream errors by provider and class.
- Label by provider and route, never by user or key, to stay inside the 10,000 free series on Grafana Cloud. Phase 2 moves to Prometheus and Grafana on the VPS.
- Traces for slow requests with spans per pipeline step and per upstream attempt (sampled).
- Structured JSON logs with a request id returned in `X-Request-Id`.

**Alerts to start with:** error rate above a threshold, p95 latency, Redis or Postgres unreachable, usage-log queue nearly full.

**Background jobs** (goroutines in the same process; split later if needed)

| Job | What it does |
| --- | --- |
| Usage writer | Drains the event channel and inserts in batches; drops oldest and counts drops if the queue overflows |
| Rollup | Builds `usage_daily` from raw events |
| Retention | Deletes old raw events and expired data |
| Quota catalog refresh | Reloads free-tier limits into memory |
| Key health check | Optional periodic test of provider credentials |

**Graceful shutdown:** stop accepting requests, let streams finish within a limit, flush the usage queue, then exit. This matters on Render, which sleeps and restarts free services.

## Code layout, config and testing

**Layout (proposed)**

```
cmd/gateway/        main: wiring, config, graceful shutdown
internal/
  api/              control plane handlers
  gateway/          /v1 handlers and the request pipeline
  auth/             gateway keys, sessions, password hashing
  provider/         adapter interface + openai (Gemini included), anthropic, mock
  router/           routes, skip rules, circuit breaker, free-first
  ratelimit/        Redis token bucket and Lua script
  cache/            exact cache, semantic cache, embedding adapters
  crypto/           AES-GCM seal and open
  store/            Postgres queries (workspace-scoped)
  usage/            event queue, writer, rollups
  telemetry/        OpenTelemetry setup
migrations/         SQL migrations
```

**Libraries to evaluate (not yet confirmed):** standard `net/http` with `chi` for routing, `pgx` for Postgres, `go-redis`, the OpenTelemetry Go SDK, `golang-migrate` or `goose`, and `golang.org/x/crypto` for password hashing. Prefer the standard library where it is enough.

**Configuration:** environment variables only (database URL, Redis URL, master key, session secret, allowed origins, feature flags such as `SEMANTIC_CACHE` and `CONTEXT_HANDOFF`). The service refuses to start if the master key is missing.

**Testing**

- Unit tests for skip rules, the token bucket, encryption and the SSE parser.
- A **mock provider** that can return slow, rate-limited, failing or mid-stream-broken responses, so fallback behavior is tested without spending any provider quota.
- Integration tests with Postgres and Redis in containers, including cross-tenant access attempts.
- Load test (for example `k6` or `vegeta`) against the mock provider to check the latency and memory targets.
- A compatibility check with the official OpenAI SDK and one IDE client.

## Build order and open decisions

**Build order**

1. Skeleton: config, Postgres and Redis connections, migrations, health endpoints, request id and logging.
2. Mock provider plus `/v1/chat/completions` passing through with streaming. This is the first thing to demo.
3. Gateway keys, encrypted provider credentials, routes and the control plane API.
4. Real adapters (OpenAI-compatible first, Gemini among them, then Anthropic).
5. Rate limiting, then routing with fallback, skip rules and circuit breaker.
6. Usage events, rollups and OpenTelemetry metrics. Gate A: free launch.
7. Free-first routing and exact cache.
8. Phase 2: semantic cache, context handoff, VPS deployment, Prometheus and Grafana, custom endpoints with the SSRF guard. Gate B: go live.

**Open decisions for you**

| Decision | Options | Suggested default |
| --- | --- | --- |
| Semantic cache timing | Phase 1 or Phase 2 | Phase 2; Phase 1 ships exact cache only |
| Context handoff timing | Phase 1 or Phase 2 | Phase 2, opt-in per route |
| Default max fallback hops | 3 or higher | 3, configurable per route |
| Sign-in method | Email and password, or OAuth | Email and password first |
| Router library | `net/http` alone or `chi` | `chi` |
| Anthropic-native endpoint | Phase 2 or later | Later |
