# 02 Embedding providers: who pays, which models, which dimensions

Type: research
Status: resolved
Blocked by: -

The spec lists FreeLLMAPI, Cloudflare Workers AI, Gemini and a small local
model on the VPS, and prefers about 1024 dimensions. Embeddings are paid for
through one of the workspace's own credentials (see Decisions so far in the
map), so the useful candidates are embedding endpoints on vendors a
credential can already name, plus any that would need a new vendor.

Answer with free keys (R19's keys work): which vendors offer embeddings on a
free tier, at which dimensions (and whether they can be shortened, as
Matryoshka-trained models allow), how fast, with what limits, and which model
family each belongs to. Failover stays inside one family so vectors remain
comparable. Say whether the local-model option is worth its memory on the
VPS.

## Answer

See [research.md](../embeddings/research.md). Of the vendors a credential can
name, only Gemini and OpenRouter embed on a free key. Default to
`gemini-embedding-2` at 1024 dimensions on the workspace's Gemini credential
(free, Matryoshka, unit vectors, about 560 ms p50), failing over inside the
family to OpenRouter's `google/gemini-embedding-2` (identical vectors, paid).
`bge-m3` (1024, open weights; OpenRouter, Cloudflare, local) is the second
family. Avoid `gemini-embedding-001`: another vector space, no `task_type`
through the OpenAI-compatible endpoint, unnormalized shortened vectors. A
workspace with no embedding-capable credential gets no semantic cache. The
local model is not worth its 2 GB. No paraphrase reached 0.95, so ticket 05
calibrates a threshold per family.

## Comments
