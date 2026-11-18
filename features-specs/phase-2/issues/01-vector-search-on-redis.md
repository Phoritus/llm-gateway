# 01 Vector search on Redis: which Redis, which index shape

Type: research
Status: resolved
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

## Answer

See [research.md](../vector-search/research.md) and its
[prototype](../vector-search/prototype/vec_test.go).

- **Image:** Redis 8 Open Source (`redis:8-alpine`, pinned to a minor version
  on the VPS). Redis Stack is folded into it. Licence: AGPLv3 of the three on
  offer, which binds only a modified Redis served over a network, so the rule
  is not to patch Redis.
- **Shape:** vector sets, one key per workspace and model,
  `vec:{workspace}:{family}:{dim}`. A search names one key, so a cross-tenant
  hit is impossible rather than filtered out. The shared index with a tag
  filter is rejected on that ground, and a query engine index per workspace
  costs 28.8 KB empty and 11 KB an entry.
- **Index type and memory:** HNSW (vector sets have no other) with Q8. That
  is about 1.8 KB an entry at 1024 dimensions, against 4.8 KB unquantized.
  The score error is at most 0.0005, and the search time is lost in the round
  trip at 5000 entries. BIN is too coarse for a threshold.
- **Cap:** evict the oldest, by a count, in one Lua script over the set, a
  `vecage:` sorted set and a `vecans:` answer hash, so a vector never outlives
  its answer. The size is ticket 05's call.
- **Model:** the key names family and dimension, Redis refuses a vector of
  another dimension into a set, and each answer record names its model.
- **Threshold:** `VSIM` scores are `(1 + cosine) / 2`, so the spec's 0.95
  cosine is `EPSILON 0.025`.

