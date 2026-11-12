# 10 Rate limiting on /v1: reserve, then reconcile

Status: ready-for-agent
Blocked by: 06

Build-order step 5, first half. `internal/ratelimit` already exists and is
used by the auth endpoints.

- Scopes: per gateway key and per workspace. The per-credential scope belongs
  with free-first (ticket 13). Units: requests per minute and estimated tokens
  per minute (about 4 characters per token).
- Reserve the estimate before the call and reconcile against reported usage
  after. The non-streaming path already asks the stream for usage; the
  streaming path must too (set `stream_options.include_usage` upstream, and
  drop the usage chunk if the client did not ask for it).
- 429 with `Retry-After` and `X-RateLimit-Remaining`.
- **Decide and record:** the spec says /v1 fails *open* with a small in-memory
  limiter when Redis is down, while the control plane deliberately fails
  closed.
- Cap in-flight streams per workspace with a semaphore.

Spec: [Rate limiting](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

From ticket 06: `/v1` is mounted in production, and a well-formed but unknown
`gwk_` key costs one unique-index Postgres lookup with nothing in front of it.
Put an IP-level limit on failed authentications ahead of the per-key limit.
