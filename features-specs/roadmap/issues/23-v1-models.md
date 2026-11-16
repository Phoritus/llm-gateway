# 23 GET /v1/models: list the workspace's route aliases

Status: needs-triage
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
