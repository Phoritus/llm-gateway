# 17 Compatibility check: the official OpenAI SDK and one IDE client

Status: resolved
Blocked by: 06

From the spec's testing list. Point the OpenAI Python SDK (`base_url=.../v1`
with a gateway key) and one IDE client at the gateway, and check that
streaming, non-streaming and an error each behave. This can start against the
mock as soon as /v1 accepts keys.

## Comments

- 2026-11-16, resolved: `make compat` (`scripts/compat.sh`) runs both clients
  against a running development gateway and the mock provider. The OpenAI
  Python SDK (openai 3.51.0) passes 13 checks: a plain answer, a stream, a
  stream with `include_usage`, the `stream()` helper, the response headers,
  and each error as the SDK's own exception class with our `code` (404
  `model_not_found`, 502, 429 with `Retry-After`, 413, 401, a stream that
  fails before its first chunk answered as a status, and one that breaks
  after it raised as `APIError` with `upstream_error`). The IDE client is the
  Continue CLI (`@continuedev/cli` 1.5.47), which runs the core and model
  config of the Continue VS Code and JetBrains extensions: a streamed answer,
  and an upstream failure, a mid-stream break and a bad key each exit 1 with
  the gateway's status and message. Neither client called anything but
  `POST /v1/chat/completions`. Continue retries a 429 on its own (three times
  here); the router's skip state answered each retry without a provider call.
  No gateway change was needed. Each error route gets its own credential in
  the harness, because a rate limit benches the whole credential for 90s.
  `GET /v1/models` does not exist; neither client needed it, but IDE model
  pickers commonly call it, so it is ticket 23.
