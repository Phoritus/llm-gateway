# Semantic cache: design (Phase 2 ticket 05)

Behind `SEMANTIC_CACHE` and a workspace setting, a request that missed the
exact cache embeds its last user message and looks for a stored answer to a
close enough message asked in the same context. A hit is replayed like an
exact hit. A miss stores its answer with its vector once the stream completes.

## Where the vectors live

`VECTOR_REDIS_URL` (`VECTOR_REDIS_URL_LOCAL` in development, the same rule as
every other dependency) names a Redis 8 with vector sets. On the VM it is the
compose `redis:8-alpine` service, `redis://redis:6379`. Everything else stays
on `REDIS_URL` (Upstash). The setting is required when `SEMANTIC_CACHE` is on
and read only then. Upstash has no vector sets, so falling back to
`REDIS_URL` would only turn every lookup into an error.

## Keys, per workspace and family (ticket 01)

| Key | Type | Holds |
| --- | --- | --- |
| `vec:{ws}:{family}:{dim}` | vector set, Q8 | element id -> vector, attribute `{"ctx":"<context hash>"}` |
| `vecage:{ws}:{family}:{dim}` | sorted set | element id by insert time (ms) |
| `vecans:{ws}:{family}:{dim}` | hash | element id -> JSON `cache.Entry` plus the exact embedding model |

Tenant isolation is the key. The workspace id is in all three names, and no
query ever names another workspace's key.

One Lua script writes an entry: `VADD ... Q8 SETATTR`, `ZADD`, `HSET`, then
pops the oldest past the cap (`ZPOPMIN`, `VREM`, `HDEL`), then `PEXPIRE`s all
three keys to `SEMANTIC_IDLE_TTL`, fixed at 7 days. A workspace that stops
asking, or a family no longer used, disappears on its own. The cap is 1000
entries per key, oldest-inserted first. A hit does not refresh age.

## The context hash

The same final question can deserve different answers under different system
prompts, histories, temperatures or routes. The context hash is SHA-256 over
what the exact key hashes (route id, route version, the normalized request
without `model`, `stream`, `stream_options`, `user`), with the last user
message's content removed. `VSIM` filters on `.ctx == "<hash>"`, so only the
last user message is allowed to vary.

## Which requests

The exact cache's `RequestCacheable` decides first, unchanged. Then:

- the process has `SEMANTIC_CACHE` on and a vector store;
- the workspace setting `semantic_cache` is on (default off);
- the request's last message is a user message with text content;
- the route has a target whose vendor serves a family (from `cache.Families`,
  preferred in the order `gemini-embedding-2`, `bge-m3`, then `mock` outside
  production). Those targets, in route order, are the embed targets. No
  such target means no semantic cache for the request.

`ResponseCacheable` decides the store, unchanged.

## Threshold

`Family.Threshold` is a cosine. It starts at 0.95 for every real family
(ticket 02 found no paraphrase above 0.945, so this favours precision, and it
is tuned on real traffic). `VSIM` takes `EPSILON (1 - c) / 2`, converted at the
call.

## On the request path

Order inside step 4: exact lookup; on an exact miss, the semantic lookup
(embed, then `VSIM vec:... VALUES <dim> ... COUNT 1 WITHSCORES EPSILON e FILTER
'.ctx == "h"'`, then `HGET vecans:...`). The embedding is bounded by its own
timeout (2s) and the Redis part by the exact cache's 250ms. Any error, a
vector without its answer, or an answer that does not decode is a miss.

A hit: `X-Cache: HIT`, zero attempts, the reservation refunded, no free quota
spent, usage `cached = true`, `gateway.cache` counts `semantic_hit`. A miss
keeps the vector it computed, and the detached store writes it with the
answer. A request that was not tried semantically records nothing new.

## API

`GET/PUT /api/workspace/settings` gains `semantic_cache_enabled`, false by
default, stored as `semantic_cache` in the settings document. It has effect
only where the deployment runs with `SEMANTIC_CACHE` on.

## Left out

Per-family calibration on labelled data, a byte cap, LRU order and the task
prefix for `gemini-embedding-2` are tuning, after traffic exists.
