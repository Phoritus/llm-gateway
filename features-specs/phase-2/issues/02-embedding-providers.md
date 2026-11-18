# 02 Embedding providers: who pays, which models, which dimensions

Type: research
Status: ready-for-agent
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

## Comments
