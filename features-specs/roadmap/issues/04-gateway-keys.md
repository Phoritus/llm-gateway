# 04 Gateway keys: table, minting, /api/gateway-keys, in-memory cache

Status: ready-for-agent
Blocked by: 01

Build-order step 3. CLAUDE.md notes "Gateway keys are not written yet".

- Migration: `gateway_keys` (id, workspace_id, hash, prefix, last_used,
  revoked_at, created_at).
- A key is a readable prefix plus a random secret, stored as a SHA-256 hash and
  returned in full once, at creation.
- `GET/POST /api/gateway-keys` and `DELETE /api/gateway-keys/{id}`, in the
  verified-email group.
- The lookup `/v1` will use (ticket 06): hash to workspace, through an
  in-memory cache backed by Postgres. Revocation must evict the entry at once
  (spec: "Revocation takes effect immediately"). With several instances, a
  short TTL on the in-process copy is the simplest answer; CLAUDE.md allows
  in-process copies of about a second.
- `last_used` must not cost a write per request (batch or throttle it).

Spec: [Security](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Data model](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
