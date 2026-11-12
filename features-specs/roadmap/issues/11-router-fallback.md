# 11 internal/router: fallback, skip rules, circuit breaker

Status: ready-for-agent
Blocked by: 07, 10

Build-order step 5, second half.

- Order the targets and drop skipped ones and those with an open breaker. Max
  hops per route (default 3), a 45s total budget.
- The skip-rule table (credential, model or provider; each with its TTL) as
  Redis keys, with a short in-process copy.
- Failover only before the first chunk. The gateway already reads the first
  chunk before committing, so a failure from `ChatStream` or from the first
  `Next` is retryable.
- Honest exhaustion: 429 with `Retry-After` if any failure was a limit, 413 if
  all were context errors, otherwise 502. Set `X-Fallback-Attempts` and
  `X-Fallback-Trail`.
- Test with routes over `mock/rate-limited`, `fail`, `break` and
  `context-too-long`.

Spec: [Routing, fallback and quotas](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

- From ticket 08's live run: the adapter's first-byte timer is a fixed 30s
  (`openai.Options.FirstByteTimeout`). Under load `gemini-3.8-flash` sent
  nothing for 30s and was answered `ErrTimeout`. That is the right call while
  failover is still legal, but a reasoning model can think longer than that
  before its first chunk. Make it a per-route (or per-target) setting here,
  where the router owns the attempt and its deadline.
