# 05 Routes: tables and /api/routes CRUD

Status: ready-for-agent
Blocked by: 03

Build-order step 3.

- Migrations: `routes` (id, workspace_id, alias, free_first, max_hops with a
  default of 3) and `route_targets` (route_id, position, credential_id, model).
- `GET/POST /api/routes` and `PUT/DELETE /api/routes/{id}`, with ordered
  targets.
- Validate that every `credential_id` belongs to the same workspace (the
  cross-tenant hole to test for), that the alias is unique per workspace, and
  that max_hops is in bounds.

Spec: [Routing](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Data model](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Control plane API](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
