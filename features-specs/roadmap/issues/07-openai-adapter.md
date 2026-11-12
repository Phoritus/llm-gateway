# 07 provider/openai: OpenAI-compatible adapter with a base URL

Status: resolved
Blocked by: 06

Build-order step 4, first adapter; it covers Groq, Cerebras, OpenRouter and
Mistral. Gaps carried over from step 2 land here:

- **Request fidelity.** `provider.ChatRequest` drops fields it does not model
  (`n`, `response_format`, `seed`, penalties, `logprobs` and so on). A
  passthrough to an OpenAI-compatible upstream must forward them. Keep the
  unknown fields (for example a raw extras map) rather than modelling OpenAI's
  whole schema.
- **Upstream 4xx.** No error kind exists for "the upstream rejected the request
  as invalid" (400 or 422). Today it would map to `ErrUpstream` and a 502,
  blaming the gateway for a client error. Add a kind and answer 400.
- **Tool calls.** `provider.Assembler` does not merge `tool_calls` deltas yet,
  which non-streaming tool-call responses need.
- An SSE line parser (the spec's testing list asks for unit tests of it),
  usage from the final chunk, and typed errors carrying `Retry-After` from
  upstream headers.
- One shared `http.Client` per provider, with a dial timeout and a
  time-to-first-byte timeout (spec, Streaming).
- `POST /api/credentials/{id}/test`, deferred from ticket 03.

Verify against the mock first, then with a real free key (ticket 19).
Spec: [Provider adapters and streaming](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

Resolved. Built against an httptest upstream; each vendor's key check was
probed with a bogus key (all five answer 401). Still unverified until ticket 19
supplies real keys: a successful stream from each vendor, and Mistral refusing
`stream_options` (the endpoint drops it on that belief).

Left open, smaller than a ticket each:

- Response fidelity. Chunks are decoded into `provider.ChatChunk` and
  re-encoded, so fields it does not model (`logprobs`, `system_fingerprint`,
  Groq's `x_groq`, reasoning deltas) are dropped on the way back. A client
  that sends `logprobs: true` now reaches the upstream but gets none back.
- Message-level unknown fields (beyond `content`, which stays raw) are dropped
  on the way in.
