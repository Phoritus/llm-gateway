# 07 provider/openai: OpenAI-compatible adapter with a base URL

Status: ready-for-agent
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
