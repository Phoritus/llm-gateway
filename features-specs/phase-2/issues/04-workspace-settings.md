# 04 Workspace settings API

Type: task
Status: ready-for-agent
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

## Comments
