# 03 Provider credentials: table, store, /api/credentials

Status: ready-for-agent
Blocked by: 02

Build-order step 3.

- Migration: `provider_credentials` (id, workspace_id, provider, ciphertext,
  nonce, key id, last4, created_at). Provider is a fixed set: `openai`
  (OpenAI-compatible, with a base URL), `gemini`, `anthropic`, and possibly
  `mock` (see ticket 06).
- Store methods are workspace-scoped like every tenant query: `workspace_id`
  comes from the session, never the body.
- `GET/POST /api/credentials` and `DELETE /api/credentials/{id}`, mounted in
  the `RequireVerifiedEmail` group in `internal/api/routes.go`. The list
  returns the last 4 characters only; the full key never leaves the server.
- `POST /api/credentials/{id}/test` needs a real adapter, so it lands with
  ticket 07, not here.
- Cross-tenant tests in the style of `internal/api/isolation_test.go`.

Spec: [Data model](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Control plane API](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Security](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
