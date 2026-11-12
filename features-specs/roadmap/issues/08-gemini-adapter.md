# 08 provider/gemini adapter

Status: resolved
Blocked by: 07

Build-order step 4. Translate OpenAI chat format to and from Gemini, with
typed errors and streaming. Spec: [Provider adapters](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

Resolved without a native adapter: Gemini goes through
`provider/openai` at Google's OpenAI-compatible endpoint, with Google's error
shape added to `classify`. See the ticket 08 line under Decisions in
[map.md](../map.md).
