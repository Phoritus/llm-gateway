# Vector search on Redis: which Redis, which index shape

Research for [Phase 2 ticket 01](../issues/01-vector-search-on-redis.md),
done on 2026-11-18. It informs ticket 05, the semantic cache, and ticket 10,
the VPS stack. The numbers come from [the prototype](prototype/vec_test.go).
It ran against a throwaway `redis:8-alpine` container (Redis 8.10.3) on the
development machine, with 1024-dimension float32 vectors.

## Answer in brief

- Run Redis 8 Open Source, the `redis:8-alpine` image compose already uses.
  Pin a minor version on the VPS. Redis Stack is not a separate product any
  more.
- Use vector sets, not the query engine. A cache is one key per workspace
  and embedding model, `vec:{workspace}:{family}:{dim}`. A search names one
  key, so another workspace's entries are unreachable by construction. There
  is no filter that a bug could forget.
- Use Q8 quantization. It costs about 1.8 KB an entry, against 4.8 KB
  unquantized and 11 KB in a query engine index. Its score error is too small
  to move an answer across a 0.95 threshold.
- Enforce the cap by evicting the oldest entry. One Lua script adds the
  vector, its age and its answer, then evicts past the cap, so a vector can
  never outlive its answer.
- The key name records the model family and the dimension. Redis refuses a
  vector of another dimension into an existing set. Changing the model writes
  to a new key.

## Which Redis, and its licence

Redis 8 merged Redis Stack into Redis Open Source. The query engine, JSON,
time series and probabilistic types ship inside it rather than as separately
versioned modules ([module lifecycle](https://redis.io/docs/latest/operate/oss_and_stack/stack-with-enterprise/modules-lifecycle/),
[what's new in 8.0](https://docs.redis.io/docs/latest/develop/whats-new/8-0)).
The container's `MODULE LIST` shows `search`, `ReJSON`, `timeseries`, `bf` and
`vectorset`, so the stock image is enough.

From Redis 8, a user picks one of three licences: RSALv2, SSPLv1 or AGPLv3
([Redis licences](https://redis.io/legal/licenses/)). The bundled modules are
under the same licence. This gateway runs Redis unmodified, as a private
backing store that no customer reaches.

- RSALv2 forbids commercializing Redis itself or offering it to others as a
  managed service. The gateway does neither.
- SSPLv1 binds a party that provides Redis as a service to others. The
  gateway doesn't.
- AGPLv3, the OSI-approved option, obliges a party that modifies Redis and
  serves the modified version over a network to publish the modifications.
  An unmodified image carries no such duty.

So AGPLv3 is the licence to name, with one rule: do not patch Redis. This is
a reading of the licence page, not legal advice. Redis points specific
questions to redis_licensing@redis.com.

Upstash, which hosts Phase 1's Redis, has no vector set or `FT.*` support.
The ticket already records this, and the prototype did not test it. The
semantic cache therefore arrives with the VPS.

## Which engine and which index shape

Redis 8 offers two vector engines, and the prototype measured both.

| Option | Isolation | Bytes per entry (5000 entries) | Cost per workspace |
| --- | --- | --- | --- |
| Vector set per workspace, Q8 | Structural: one key per cache | 1775 | about 3.8 KB with one entry |
| Vector set per workspace, unquantized | Structural | 4848 | |
| Vector set per workspace, BIN | Structural | 877 | |
| Query engine index per workspace, HNSW | Structural: one index per prefix | 11077 | 28.8 KB per empty index, plus an `FT.CREATE` to manage |
| Query engine index per workspace, FLAT | Structural | 10666 | as above |
| One shared query engine index with a workspace tag | A filter every query must remember | as HNSW | none |

The shared index is ruled out by the ticket's own test. Its isolation is one
`@workspace:{…}` clause that every query has to carry, so a bug that drops it
serves another tenant's answer. That makes a cross-tenant hit unlikely, not
impossible. A filtered HNSW search also loses recall when one tenant is a
small share of a large graph.

Both per-workspace options are isolated structurally, and vector sets are
cheaper on every measure. An index per workspace costs 28.8 KB before its
first entry. It needs an `FT.CREATE` on the first write and an
`FT.DROPINDEX` on delete, and it stores each vector twice, once in the hash
and once in the index. A vector set is created by its first `VADD` and
removed by `UNLINK`, like any other key.

The per-entry figures are deltas of `used_memory`. For the query engine they
include the hash that holds the raw vector, which that design needs anyway.
For vector sets they include the HNSW graph and the element name. They
exclude the cached answers, which are the same size under every option.

### HNSW or FLAT

Vector sets are HNSW only. The choice matters little at these sizes. At 5000
entries, the search time is lost in the round trip for both engines. In
query engine terms, FLAT saves no memory: 10.7 KB against 11.1 KB.

| Query (5000 entries) | p50 | p99 |
| --- | --- | --- |
| `PING`, the round trip alone | 0.52 ms | 1.0 ms |
| `VSIM COUNT 1` | 1.04 ms | 1.6 ms |
| `VSIM COUNT 1 NOTHREAD` | 0.53 ms | 1.1 ms |
| `FT.SEARCH … KNN 1` | 0.53 ms | 1.1 ms |

Each figure is a median of 300 queries, steady across three runs.
`VSIM`'s extra 0.5 ms is the handoff to a worker thread, and `NOTHREAD`
removes it. Keep the default thread anyway. Half a millisecond is noise next
to the embedding call that precedes every lookup. The thread stops a large
set from stalling the Redis main thread, which the rate limiter also uses.

### Quantization

Q8 is the default and the right one. On 80 near-duplicate queries at cosines
from 0.93 to 0.99, against 2000 stored entries, its largest score error was
0.0005 score units. That is 0.001 in cosine, well inside any threshold
margin. The stored entry was the top hit every time. BIN was off by up to
0.1 score units, enough to turn a 0.93 match into a hit at 0.95 or to hide a
0.99 match, so it is unusable for a threshold cache.

### Scores and the threshold

`VSIM … WITHSCORES` returns `(1 + cosine) / 2`. Cosine 1 scores 1, 0.95
scores 0.975, 0 scores 0.5 and -1 scores 0. `EPSILON` is a maximum distance
in the same units, so `VSIM` returns only entries scoring at least
`1 - EPSILON`. A cosine threshold `c` is therefore `EPSILON (1 - c) / 2`, and
the spec's starting 0.95 is `EPSILON 0.025`. Ticket 05 should keep the
threshold configured in cosine, the unit the spec and the embedding vendors
use, and convert it at the call.

## The per-workspace size cap

Evict the oldest rather than refuse new entries. A refusing cache freezes on
whatever it held first. An evicting one keeps the answers to what the
workspace asks now.

A vector set has no order, so each cache keeps two companions:

- `vecage:{workspace}:{family}:{dim}`, a sorted set of element ids by time;
- `vecans:{workspace}:{family}:{dim}`, a hash of element id to the stored
  answer.

One Lua script adds the vector, its age and its answer, then pops the oldest
past the cap with `ZPOPMIN`, `VREM` and `HDEL`. The script runs atomically, so
a crash cannot leave a vector whose answer is gone. The prototype stored 150
entries at a cap of 100. All three structures held exactly 100, the 50
oldest were gone and the oldest survivor was kept. Rewriting an entry's age
on a hit would turn this into least-recently-used for one more `ZADD`. Ticket
05 should decide that.

The cap is a count, and its size is ticket 05's call. Vectors cost about
1.8 KB each, so 10 000 entries are 18 MB of vectors. The answers are the
larger part: the exact cache allows up to 256 KiB an entry. A byte cap may
therefore matter more than a count cap.

## Recording the model

The spec asks for the model family and dimensions on each entry and on the
index. Carry both in the key: `vec:{workspace}:{family}:{dim}`. A set fixes
its dimension at the first `VADD`, `VDIM` reports it, and a vector of another
dimension is refused (`ERR Vector dimension mismatch - got 768 but set has
1024`). A change of model therefore cannot mix vectors: it writes to a new
key, and the old cache is removed with `UNLINK`. Each answer record in
`vecans:` should also name the exact model, so a replay can be checked
against the request's current settings. `VSETATTR` is not needed for this,
because nothing filters on the model.

## Open for ticket 05

- The cap's size, and whether it counts entries, bytes or both.
- Eviction order: oldest inserted, or least recently hit.
- Which key holds a workspace's caches when its model changes, and whether
  the old one is removed at once or left to expire.
