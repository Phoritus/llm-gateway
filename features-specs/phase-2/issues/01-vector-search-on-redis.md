# 01 Vector search on Redis: which Redis, which index shape

Type: research
Status: ready-for-agent
Blocked by: -

The spec puts the semantic index in Redis (`vec:{workspace}`, no TTL, size
capped) and names Redis Stack. Upstash, where Phase 1's Redis lives, has no
`FT.*` commands, so the semantic cache arrives with the VPS (ticket 10).
Answer, with a prototype against a container:

- Which image: Redis 8's built-in query engine or Redis Stack, and its
  licence for a hosted service.
- One index per workspace, or one index with a workspace tag filter. The
  answer has to make cross-tenant hits impossible, not just unlikely.
- HNSW or FLAT at our sizes, the memory per entry at 1024 float32
  dimensions, and how the per-workspace size cap is enforced (count and evict
  oldest, or refuse new entries).
- How an entry records its embedding model family and dimensions, so that
  changing model starts a new index, as the spec requires.

Spec: [Caching and embeddings](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
