# Free-first routing and the exact cache (ticket 13)

Build-order step 7. Two features that share one migration and one pipeline
step each: the exact cache is step 4 of the request pipeline, free-first is
part of step 5's target ordering.

## Data

Migration `00007_cache_and_free_quota.sql`:

- `routes.cache text NOT NULL DEFAULT 'auto' CHECK (cache IN ('auto','always','off'))`.
  `auto` caches only a request whose `temperature` is exactly 0, `always`
  caches every cacheable request on the route, `off` never caches. This is the
  spec's "only when temperature is 0 or the user turns caching on", plus the
  off switch. There is no workspace-wide switch: there is no workspace
  settings API yet, and a column nothing can set is dead. It lands with the
  Phase 2 settings endpoints.
- `usage_daily.cache_hits bigint NOT NULL DEFAULT 0`, added by
  `store.WriteBatch` from the rows it inserts with `cached = true`, so the
  dashboard can show a hit rate without scanning raw events.
- `free_quota_catalog (vendor text, model text, rpm integer, rpd integer,
  tpm integer, tpd integer, PRIMARY KEY (vendor, model))`. Each limit is
  nullable: a vendor that publishes no daily token limit has none. Every
  non-null limit is positive. Seeded with the free tiers known for the
  vendors in `openai.Endpoints`; the values are data the upstream changes, so
  correcting one is an `UPDATE`, not a release.

`store.ResolvedRoute` gains `FreeFirst bool`, `Cache store.CacheMode` and
`Version int64` (the route's `updated_at` in Unix nanoseconds), read by the
existing single query.

## Exact cache (`internal/cache`)

- **Key**: `cache:{workspace}:{hex sha256}`. The hash covers the route id, the
  route's `Version`, and the normalized request. Normalizing: drop `model`
  (the alias resolved to the route already), `stream`, `stream_options` and
  `user`, then decode the rest with `UseNumber` and re-encode, so key order and
  whitespace inside `messages`, `stop` or `Extra` fields cannot split one
  request into two entries. `Version` means a PUT that changes the chain
  stops serving answers from the old chain, with no invalidation step.
- **Cacheable request**: the route's mode allows it, and the request has no
  `tools`, `tool_choice`, `functions` or `function_call`, and no message with
  role `tool` or `function` or with `tool_calls`.
- **Cacheable response**: the stream ended with `io.EOF` (outcome `ok`), every
  choice has a finish reason, and no choice carries tool calls. Stored as the
  assembled `ChatResponse` JSON plus the vendor and model that answered, only
  when it is at most 256 KiB, with `CACHE_TTL` (default 1h). The write is
  detached from the request, like the rate-limit settle.
- **Lookup** runs after the route resolves and before `Router.Run`. A Redis
  error or a value that does not decode is a miss and is logged; the cache
  never fails a request.
- **Hit**: the stored response becomes a `provider.Stream` (one chunk per
  choice carrying role, content and finish reason, then a usage-only chunk),
  and the handler's existing streaming and non-streaming paths send it. So a
  streaming client is replayed a stream and the code that formats one exists
  once. A hit spends no provider tokens: the token reservation is refunded in
  full and the usage event records zero tokens, `cached = true`, and the
  vendor and model that produced the stored answer. `X-Cache: HIT`;
  `X-Cache: MISS` on a cacheable request that missed; no header otherwise.
  Fallback headers report zero attempts.
- **Metric**: `gateway.cache` counter labelled `result` in `hit`, `miss`,
  `stored`, `skipped` (not cacheable), `error`. A hit's request metrics carry
  vendor `cache`, so a hit's near-zero time to first token does not pull down
  a real vendor's histogram.
- `CACHE_TTL=0` turns the exact cache off everywhere.

## Free-first (`internal/quota`)

- `Catalog`: the `free_quota_catalog` rows in memory, keyed by vendor and
  model, loaded at startup and reloaded every 10 minutes by a goroutine (the
  spec's "quota catalog refresh" job). A failed load keeps the previous
  catalog and logs; an empty catalog means free-first keeps the user's order.
- **Buckets**: per credential and model, one `ratelimit.Bucket` per non-null
  limit. Scopes are fixed names (`free:rpm`, `free:rpd`, `free:tpm`,
  `free:tpd`); the id is `{credential}:{model}`. Daily buckets carry a 25h TTL
  so an idle hour does not reset a day's count.
- **Spend**: after a request answered by a target with a catalog entry,
  `Settle` one request and the reported total tokens (the estimate when none
  was reported) on its buckets, detached. Every route spends, free-first or
  not: the quota belongs to the credential, whatever route used it. A cache
  hit spends nothing.
- **Order**: on a free-first route only, `Prioritize` reads the targets'
  buckets without spending (`Peek`), then moves the targets that have a
  catalog entry and room for one more request and the estimated tokens to the
  front, keeping the user's order within each group. Targets with no entry or
  no room keep their order behind. Exhausted free targets are demoted, not
  dropped: the counts are this gateway's view, not the upstream's, and the
  router's skip rules handle a real 429. A `Peek` error keeps the user's order.
- `Peek` is new on `ratelimit.Limiter`, `Local` and `FailOpen`: the levels the
  buckets would have now, read-only (no key is created or refreshed, no
  rejection counted).

The router itself is unchanged: it is handed the targets in the order to try.
