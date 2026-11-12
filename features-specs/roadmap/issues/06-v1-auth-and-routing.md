# 06 /v1: authenticate gateway keys, resolve model to route, mount in production

Status: resolved
Blocked by: 04, 05

Pipeline steps 1 and 2 for real; this finishes build-order step 3. Run
`architect` first: this is where the step 2 interface meets real tenancy.

- Bearer gateway key to workspace through ticket 04's cache; 401 in the OpenAI
  envelope otherwise.
- Resolve `model` to a workspace-owned route alias; 404 `model_not_found` for
  an unknown alias. Until the router exists (ticket 11), call only the route's
  first target.
- `gateway.Deps` grows from one `Provider` to adapters looked up by the
  credential's provider kind, with the decrypted credential passed per call
  (`provider.Credential`).
- Decide how `mock` fits. A `mock` provider kind that works in development and
  tests keeps fallback testable end to end; production should refuse it.
- Remove the `!cfg.IsProduction()` guard and its warning around `/v1` in
  `cmd/gateway/main.go`, and update the state table in CLAUDE.md.
- `/v1` enforces email verification through the key's workspace (CLAUDE.md,
  "Email verification is required").

## Comments

Note from ticket 04: `auth.GatewayKeys.Authenticate` caches hits only, so a
well-formed but unknown `gwk_` token costs one indexed Postgres query. Auth runs
before the per-key rate limit, so put an IP-level limit ahead of it or add a
short negative cache. The handler strips `Bearer ` itself and maps
`ErrInvalidGatewayKey` to the OpenAI-shaped 401.
