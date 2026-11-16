# 23 GET /v1/models: list the workspace's route aliases

Status: resolved
Blocked by: -

`/v1` serves only `POST /chat/completions`. Ticket 17's clients (the OpenAI
Python SDK and the Continue CLI with an explicit model) never asked for
anything else, but IDE model pickers and some SDK helpers call
`GET /v1/models` to list what a key can use, and get our 404 `not_found`.

If built: authenticate the gateway key as `/v1/chat/completions` does, and
answer the OpenAI list shape (`{"object":"list","data":[{"id":<alias>,
"object":"model","created":<route created_at>,"owned_by":"llm-gateway"}]}`)
with the workspace's route aliases, never the upstream model names behind
them. It costs a Postgres query per call, so it needs the per-IP and per-key
limits too. Triage: is there a client we want to support that breaks
without it?

## Comments

Triage (2026-11-16): ready-for-agent. Open WebUI calls `GET /models` to verify
a connection and fill its picker, and JetBrains AI Assistant's
OpenAI-compatible provider and LibreChat (`fetch: true`) list models the same
way. Each of those breaks or shows an empty picker without it.

## Answer

`GET /v1/models` lists the workspace's route aliases in OpenAI's list shape,
`owned_by: "llm-gateway"`, `created` from the route's `created_at`, ordered by
alias. It reads `routes` alone (`store.RouteAliases`), so no credential is
opened and no upstream model name can leak. It authenticates exactly as chat
does, through the same `authenticate` (a failed key counts against the
address), and an admitted listing spends one request from the key's and the
workspace's request buckets, no tokens and no in-flight slot. It records no
usage event, since no route was resolved. `make compat` now calls the OpenAI
SDK's `models.list()`. `GET /v1/models/{id}` is not served; add it when a
client is found that needs it.
