# 19 Create provider API keys on free tiers

Status: ready-for-human
Blocked by: 03

Sign up at the providers you want (for example Groq, Cerebras, OpenRouter,
Gemini, Mistral) and create API keys. Enter them through the dashboard or
`POST /api/credentials` once ticket 03 exists, never into a committed file or
a chat. They are needed to verify tickets 07 to 09 against a real upstream.

## Comments

- 2026-11-16: `make live` checks every vendor whose key is in the env file
  through a running dev gateway: credential test, a plain and a streamed
  answer, then failover from a bad key, an exact-cache hit and the usage API.
  Groq (`openai/gpt-oss-20b`), Gemini (`gemini-3.5-flash-lite`), Cerebras
  (`gpt-oss-120b`) and OpenRouter (`nvidia/nemotron-3.5-lightning:free`) pass
  it all. OpenRouter's `google/gemma-4-31b-it:free` was rate limited by its
  upstream (Google AI Studio) at the time, which the gateway reported
  correctly. The OpenAI key has no credit: its 429 says `type:
  insufficient_quota` with the new `code: credit_balance_exhausted`, which
  the adapter read as a rate limit until it learned to check `type` too.
  Still unchecked against a real upstream: Anthropic (no key, so ticket 09's
  version header and full stream) and Mistral (paid only).
