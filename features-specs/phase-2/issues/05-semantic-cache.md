# 05 Semantic cache in the pipeline

Type: task
Status: resolved
Blocked by: 01, 03, 04

Behind `SEMANTIC_CACHE`, and only for a workspace that turned it on (04):
after an exact-cache miss, embed the last user message, search the
workspace's index (01) and return the stored answer above the threshold,
starting at 0.95 and tuned on real traffic. A miss stores the completed
answer with its vector, model family and dimensions. The same never-cache
rules as the exact cache, through `cache.RequestCacheable` and
`ResponseCacheable`, so there is still one place that decides.

A hit is replayed like an exact hit (`X-Cache: HIT`, the token reservation
refunded, recorded with `cached = true`), with the result labelled
`semantic_hit` on `gateway.cache`. An embedding failure or timeout is a miss.
Answers are per workspace only: an integration test must try a cross-tenant
hit.

## Answer

Built to [the design](../../semantic-cache/design.md). `cache.Semantic` keeps, per workspace and family, a Q8 vector set with an age sorted set and an answer hash, written by one Lua script (cap 1000, oldest first, 7-day idle expiry) in the Redis 8 named by `VECTOR_REDIS_URL`. A vector carries a context hash (the exact key's hash without the last message), and `VSIM` filters on it, so only the last user message may vary. The embed targets are the route's own targets, the family the first of `gemini-embedding-2`, `bge-m3` (and `mock` outside production) that one of them serves. The workspace opts in with `semantic_cache_enabled`. Thresholds are 0.95 for the real families, uncalibrated, and 0.9 for the mock, above its 8-bucket collisions (unrelated text scored 0.80).

Verified end to end through the binary on Redis 8.10.3: a reworded question hits with the first answer; another system prompt, an unrelated question, another workspace and a workspace that did not opt in all miss.

Spec: [Caching and embeddings](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

- 2026-11-18: ticket 01 settled the index: vector sets, one key per workspace and model, Q8, oldest-first eviction in one Lua script. Left open here: the cap's size and unit, LRU or insertion order, and when an old model's key is removed. See [research.md](../vector-search/research.md).

- 2026-11-18: ticket 02 answered which embedders: see [research.md](../embeddings/research.md) and its open questions for this ticket.

- 2026-11-23: blocked on the owner. Redis stays on Upstash, and Upstash has no vector sets (ticket 01's research). The vectors need another home. The options: keep the `redis:8-alpine` container on the VM for `vec:` keys only, under a second URL; use Upstash Vector, a separate product with its own API; or a vector column in Azure Postgres (pgvector). Recommended: the VM container, because it is already running and ticket 01's design is unchanged. A lost vector is only a miss.
- 2026-11-23: decided by the owner: the vectors live in the `redis:8-alpine` container on the VM, ticket 01's design unchanged. Everything else stays on Upstash. The container gets its own URL setting (say `VECTOR_REDIS_URL`), read only when `SEMANTIC_CACHE` is on, so `REDIS_URL` keeps pointing at Upstash.
