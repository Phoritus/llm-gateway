# 13 Free-first routing and exact cache

Status: ready-for-agent
Blocked by: 12

Build-order step 7: `free_quota_catalog`, per-credential buckets, free-first
ordering (opt-in per route), and the exact-match cache in Redis. Never cache
tool calls, errors, or workspaces that turned caching off. Replay hits as a
stream; `provider.Assembler` can build the stored value while the response
streams. Spec: [Caching](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Routing](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
