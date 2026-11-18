# 05 Semantic cache in the pipeline

Type: task
Status: ready-for-agent
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

Spec: [Caching and embeddings](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
