# 04 Workspace settings API

Type: task
Status: resolved
Blocked by: -

`workspaces.settings` is in the spec's data model and nothing reads it yet.
Ticket R13 deferred a workspace-wide cache switch until a settings API
exists, and the spec's "never cache workspaces that turned caching off"
needs one. Phase 2 hangs several settings off it: the semantic cache
(on or off, threshold, which credential and model embed), and later prompt
logging (17) and budgets (18).

Build `GET` and `PUT /api/workspace/settings` behind the verified-email gate,
a typed and validated settings struct (unknown fields refused, as the rest of
/api does), and the workspace-wide cache switch read by the /v1 pipeline.
Decide whether settings reach /v1 through `ResolveRoute`'s query or the key
cache; either way a change must apply within a second or two.

## Answer

`GET` and `PUT /api/workspace/settings`, inside the verified-email group,
take and return `{"cache_enabled": true}`. A PUT replaces the whole document,
so a field left out takes its default, and an unknown field is refused, as
everywhere in /api. `store.WorkspaceSettings` is the typed jsonb: each
field's zero value is its default (`CacheOff`, not `CacheEnabled`), so a row
from before a field existed and a `ResolvedRoute` built without settings both
mean the defaults.

Settings reach /v1 through `ResolveRoute`'s query, joined on the workspace's
primary key, not the key cache: the change applies to the next request with
no TTL to reason about, and auth stays ignorant of settings.
`cache.RequestCacheable` now takes the resolved route and refuses when the
workspace turned caching off, so it is still the one place that decides.

Turning the cache off neither deletes nor re-keys stored answers: while off,
nothing is served or stored; turned back on, answers still inside `CACHE_TTL`
are served again. Ticket 05 adds the semantic settings to the same struct.
Nested settings objects will need `decodeJSON`'s unknown-field check to
recurse; today's document is flat.

## Comments
