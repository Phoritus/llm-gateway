# Embedding providers: who pays, which models, which dimensions

Research for [Phase 2 ticket 02](../issues/02-embedding-providers.md), done
on 2026-11-18. It informs ticket 03, the embedding adapter, and ticket 05, the
semantic cache. The live numbers come from [the probe](prototype/probe.py),
run with the free keys in `.env.dev` from the development machine, so the
latencies include that machine's distance to each vendor. They are a dozen
calls each, not a benchmark.

## Answer in brief

- Of the vendors a credential can name today, two serve embeddings on a free
  key: **Gemini** and **OpenRouter**. Groq and Cerebras serve none, Anthropic
  has no embedding model, Mistral's free mode has no API access, and OpenAI
  charges for every embedding.
- Default the semantic cache to **`gemini-embedding-2` at 1024 dimensions**,
  through the workspace's Gemini credential. It is free, Matryoshka-trained,
  returns unit vectors at 1024, and works through the OpenAI-compatible
  endpoint the gateway already calls. It took 560 ms at p50.
- Fail over inside the family to **OpenRouter's `google/gemini-embedding-2`**.
  The probe got identical vectors from both endpoints, cosine 1.00000. That
  hop is paid, at $0.20 per million tokens, so it needs OpenRouter credit.
- A second family, **`bge-m3`** (1024 dimensions, open weights), is served by
  OpenRouter for $0.01 per million tokens, by Cloudflare Workers AI (a new
  vendor, free up to about 9 million tokens a day), and by a local model on
  the VPS. It suits a workspace whose only embedding-capable credential is
  OpenRouter.
- OpenRouter's free embedding models, NVIDIA's Nemotron, are unusable
  inline: 7.7 s at p50 and up to 30 s, and 50 requests a day on an account
  that has bought less than $10 of credit.
- A workspace with no credential that names an embedding family gets no
  semantic cache. The setting is unavailable to it rather than quietly
  paid for by the platform.
- The local model is not worth its memory. bge-m3 on two CPU cores held
  2.0 GB resident, after its default settings were killed at 4 GB, and took
  61 ms for a short question but a full second for a 290-token message. It
  would also be the platform paying for embeddings, which the map's
  decision rules out.
- Every family needs its own threshold. No paraphrase pair the probe tried
  reached the spec's 0.95, so ticket 05 must calibrate per family before
  turning the cache on.

## Who serves embeddings

The vendors a credential can name are those in `provider.Vendor`:
`openai`, `groq`, `cerebras`, `openrouter`, `mistral`, `gemini` and
`anthropic`.

| Vendor | Embeddings on a free key | Models | Notes |
| --- | --- | --- | --- |
| Gemini | Yes | `gemini-embedding-2`, `gemini-embedding-001` | Free of charge on the free tier ([pricing](https://aistudio.google.com/docs/pricing)) |
| OpenRouter | Free models only, and slow | 38 models from `GET /embeddings/models`, 3 of them `:free` | Paid models need credit |
| OpenAI | No | `text-embedding-3-small`, `-3-large` | $0.02 and $0.13 per million tokens ([pricing](https://developers.openai.com/api/docs/pricing)) |
| Mistral | No | `mistral-embed` (1024), `codestral-embed` | Free mode "doesn't include API access" ([usage and limits](https://docs.mistral.ai/admin/billing-usage/usage-limits)) |
| Groq | No | none | `/models` lists no embedding model; `POST /embeddings` exists and answers `model_not_found` |
| Cerebras | No | none | `/models` lists none; `POST /embeddings` is a 404 |
| Anthropic | No | none | "Anthropic does not offer its own embedding model" and points to Voyage AI ([embeddings](https://platform.claude.com/docs/en/build-with-claude/embeddings)) |

The [Groq models page](https://console.groq.com/docs/models) and
[Cerebras models page](https://inference-docs.cerebras.ai/models/overview)
list no embedding model either. Groq's Cloudflare front refuses Python's
default user agent with `403 error code: 1010`, so the probe sends its own.
The gateway's Go client already does.

OpenAI's "Free" usage tier is a $100 a month spending cap for an allowed
geography, not free tokens ([rate limits](https://developers.openai.com/api/docs/guides/rate-limits)).
Mistral's 2024 free API tier is gone: creating an API key "require[s]
billing activation and pay-as-you-go".

## The candidates

### Gemini

The Gemini API offers two text embedding models, and their vectors are not
comparable: "The embedding spaces between `gemini-embedding-001` and
`gemini-embedding-2` are **incompatible**" ([embeddings](https://aistudio.google.com/docs/embeddings)).
Each is its own family.

| | `gemini-embedding-001` | `gemini-embedding-2` |
| --- | --- | --- |
| Input | Text, 2048 tokens | Text, image, audio, video, PDF; 8192 tokens |
| Dimensions | 3072 default, 128 to 3072, Matryoshka | The same |
| Shortened vectors normalized | No: "you must manually normalize non-3072 dimensions" | Yes: it "auto-normalizes truncated dimensions" |
| Task | `task_type`, native API only | No `task_type`; the instruction goes in the text, `task: … \| query: …` |
| Paid price | $0.15 per million tokens | $0.20 per million text tokens |

The probe confirmed each row it could reach.

| Probe | Dimensions | Norm | cos(paraphrase) | cos(unrelated) |
| --- | --- | --- | --- | --- |
| `001`, OpenAI-compatible | 3072 | 1.0000 | 0.754 | 0.491 |
| `001`, OpenAI-compatible | 1024 | **0.6140** | 0.731 | 0.439 |
| `001`, OpenAI-compatible | 768 | 0.5818 | 0.745 | 0.453 |
| `001`, native, `SEMANTIC_SIMILARITY` | 1024 | 0.6237 | **0.945** | 0.661 |
| `2`, OpenAI-compatible | 3072 | 1.0000 | 0.915 | 0.490 |
| `2`, OpenAI-compatible | 1024 | **1.0000** | 0.925 | 0.520 |

The paraphrase pair is "How do I reset my password?" against "What are the
steps to change a forgotten password?". The unrelated text is a recipe line.

`gemini-embedding-001` is the worse fit. Through the OpenAI-compatible
endpoint it gets no task type, and without one a close paraphrase scores
0.73. Its shortened vectors also arrive at length 0.61, so the adapter would
have to normalize them. Redis vector sets normalize on `VADD` and score by
cosine, so this is harmless to search, but it is one more rule to get right.
Using it well means the native `batchEmbedContents` API, a second wire format
for one vendor.

`gemini-embedding-2` needs neither. It is reached at
`https://generativelanguage.googleapis.com/v1beta/openai/embeddings`, the
base URL already in `openai.Endpoints` ([OpenAI compatibility](https://aistudio.google.com/docs/openai)),
with `dimensions: 1024`. Its task instruction is plain text, so the adapter
prefixes the message the same way on every endpoint of the family. The probe
sent no prefix, which ticket 05's calibration should change (see the open
questions).

**Latency.** Ten sequential one-sentence calls at 1024 dimensions:
`gemini-embedding-2` p50 560 ms, max 657 ms; `gemini-embedding-001` p50
522 ms, max 1084 ms.

**Limits.** Google does not publish free-tier embedding limits. Limits are
"applied per project, not per API key", daily quotas reset at midnight
Pacific, and each project's are shown in AI Studio ([rate limits](https://aistudio.google.com/docs/rate-limits)).
A forum reply gives `gemini-embedding-001`'s as 100 requests a minute,
30 000 tokens a minute and 1000 requests a day ([forum](https://discuss.ai.google.dev/t/gemini-embedding-free-tier-documentation/112553)).
That is not an official source, and nothing was found for
`gemini-embedding-2`. If 1000 a day holds, a free Gemini key covers about
1000 exact-cache misses a day before the semantic cache starts missing on
429s.

**Data.** For `gemini-embedding-2`, free-tier content is used to improve
Google's products and paid-tier content is not ([pricing](https://aistudio.google.com/docs/pricing)).
Embedding the last user message on a free key therefore sends prompt text to
Google under those terms. The spec says prompt text is not stored unless the
user opts in. A workspace already sends the same text to Gemini when it
routes chat there, but ticket 04's setting should say so.

### OpenRouter

OpenRouter serves embeddings at `POST /api/v1/embeddings` and lists them at
`GET /api/v1/embeddings/models` ([embeddings](https://openrouter.ai/docs/api/reference/embeddings)).
The probe's key listed 38 models, among them `google/gemini-embedding-2`,
`google/gemini-embedding-001`, `baai/bge-m3`, `openai/text-embedding-3-small`,
`mistralai/mistral-embed-2312`, `qwen/qwen3-embedding-4b` and the Voyage 4
models. Prices per million tokens, from that listing: bge-m3 $0.01,
`text-embedding-3-small` $0.02, `gemini-embedding-001` $0.15,
`gemini-embedding-2` $0.20.

Three are free: `nvidia/nemotron-3.5-embed-1b:free`,
`nvidia/nemotron-3.5-embed-8b:free` and `nvidia/nemotron-3-embed-1b:free`.
Free models are held to 20 requests a minute, and to 50 a day for an account
that has bought less than 10 credits ([limits](https://openrouter.ai/docs/api/reference/limits)).
The 1B model returned 2048 dimensions, took a 1024 `dimensions` request, and
scored the paraphrase 0.69 and the unrelated text 0.08. Five sequential calls
took p50 7.7 s and up to 29.8 s, and two of the probe's calls timed out at
30 s. A cache lookup that slow is worse than no cache.

The paid models answered on the probe's free-tier key, which has never
bought credit (`is_free_tier: true`) but has a $50 limit set. The probe's
paid calls cost $0.0000012 in all. Do not read this as free use: ticket 03
should treat a 402 from OpenRouter as a credential without credit, a skip
like any other.

| Probe | Dimensions | Norm | cos(paraphrase) | cos(unrelated) | p50 / max |
| --- | --- | --- | --- | --- | --- |
| `google/gemini-embedding-2` | 1024 | 1.0000 | 0.925 | 0.520 | 839 / 1082 ms (3 calls) |
| `baai/bge-m3` | 1024 | 1.0000 | 0.886 | 0.426 | 989 / 1677 ms (5 calls) |
| `nvidia/nemotron-3.5-embed-1b:free` | 1024 | 1.0000 | 0.694 | 0.087 | 7706 / 29 835 ms (5 calls) |

**One family across two vendors.** The same three texts embedded by Gemini
and by OpenRouter's `google/gemini-embedding-2` at 1024 dimensions gave
same-text cosines of 1.00000, 1.00000 and 1.00000. OpenRouter passes the
request through to Google, so the two endpoints make one family and can fail
over to each other.

### New vendors

None of these can be named by a credential today. Each would be a new
`provider.Vendor`, which the BYOK rule requires: the workspace brings that
key too.

- **Cloudflare Workers AI.** 10 000 neurons a day free, then $0.011 per 1000
  ([pricing](https://developers.cloudflare.com/workers-ai/platform/pricing/)).
  `@cf/baai/bge-m3` costs 1075 neurons per million input tokens, so the free
  allowance is about 9.3 million tokens a day. `@cf/google/embeddinggemma-2`
  costs 154 and `@cf/qwen/qwen3-embedding-0.6b` 1075. Text embeddings are
  limited to 3000 requests a minute ([limits](https://developers.cloudflare.com/workers-ai/platform/limits/)).
  It has an OpenAI-compatible `/v1/embeddings`, at
  `https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1`
  ([OpenAI compatibility](https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/)),
  so the credential is an API token plus an account id, the first credential
  to need two values. Its models are open-weight ones, so it shares families
  with a local model and with OpenRouter: bge-m3 is on all three.
- **Voyage AI**, Anthropic's recommendation. The first 200 million tokens of
  `voyage-4-large`, `voyage-4` and `voyage-4-lite` are free per account
  ([pricing](https://docs.voyageai.com/docs/pricing)). All take 256, 512,
  1024 (default) or 2048 dimensions, and "All embeddings created with the 4
  series are compatible with each other" ([embeddings](https://docs.voyageai.com/docs/embeddings)),
  including the open-weight `voyage-4-nano`
  ([model card](https://huggingface.co/voyageai/voyage-4-nano), 340M
  parameters, Apache 2.0). That makes it the only family with a local member
  whose API members differ in size. Rate limits are listed only from Tier 1,
  which "requires you add a payment method" ([rate limits](https://docs.voyageai.com/docs/rate-limits)).
  Voyage 4 is also on OpenRouter.
- **Jina AI.** New keys come with an unstated amount of free tokens, at 100
  requests and 100 000 tokens a minute. The v5 models support Matryoshka
  truncation down to 32 dimensions ([embeddings](https://jina.ai/embeddings/)).
- **Cohere.** Trial keys allow 2000 Embed inputs a minute but only 1000 calls
  a month across the API, and are described as evaluation keys
  ([rate limits](https://docs.cohere.com/docs/rate-limits)).
- **Nomic.** No current free API allowance was found; its plans include
  embedding tokens from the paid Plus plan ([pricing](https://atlas.nomic.ai/pricing)).

**FreeLLMAPI** is not a vendor. It is a self-hosted, MIT-licensed router that
the user runs on their own machine with their own provider keys, "local-first
and single-user by design", and labelled "Personal experimentation only"
([repository](https://github.com/tashfeenahmed/freellmapi)). It exposes
`/v1/embeddings` across the free tiers it knows. It does for one person what
this gateway does for a workspace, so it adds no free capacity: its upstreams
are the ones listed here. It could at most be reached as a custom endpoint
(ticket 09), which is a user's choice and not a family the gateway can
reason about.

## Families

A family is a set of endpoints that return the same vector for the same
text, so one cache key's vectors can come from any of them. The key from
ticket 01, `vec:{workspace}:{family}:{dim}`, names it.

| Family | Dimensions | Endpoints a credential reaches today | Other endpoints | Free |
| --- | --- | --- | --- | --- |
| `gemini-embedding-2` | 128 to 3072, Matryoshka, unit vectors | Gemini, OpenRouter | | Gemini free tier |
| `gemini-embedding-001` | 128 to 3072, Matryoshka, not normalized | Gemini, OpenRouter | | Gemini free tier |
| `bge-m3` | 1024 only | OpenRouter | Cloudflare, local | Cloudflare's allowance, or local |
| `text-embedding-3-small` | 1536, shortened by `dimensions` | OpenAI, OpenRouter | | No |
| `mistral-embed` | 1024 only | Mistral, OpenRouter | | No |
| `voyage-4` | 256 to 2048 | OpenRouter | Voyage, local `voyage-4-nano` | Voyage's 200M tokens |
| `qwen3-embedding-0.6b` | 32 to 1024, Matryoshka | | Cloudflare, local | Cloudflare's allowance, or local |

bge-m3's dimension is 1024 with no Matryoshka training
([model card](https://huggingface.co/BAAI/bge-m3)). Qwen3-Embedding-0.6B
takes "user-defined output dimensions ranging from 32 to 1024"
([model card](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B)).

For open weights, "the same family" holds only as far as the endpoints run
the same weights at a similar precision. For bge-m3 the probe checked it:
the same three texts embedded by OpenRouter and by a local
text-embeddings-inference container (fp32 ONNX) gave same-text cosines of
1.00000, 1.00000 and 1.00000. A quantized GGUF build, as Ollama serves, was
not checked and may drift further.

## Dimensions

1024 is the right default, and every recommended family offers it.
`gemini-embedding-2` shortens to 1024 by Matryoshka and normalizes, and bge-m3
is 1024 natively. Ticket 01 measured 1775 bytes an entry for a 1024-dimension
vector under Q8, against 4848 unquantized. A Q8 vector costs one byte a
dimension, so 768 would save about 256 bytes of the vector itself, around a
seventh of the entry, and nothing of the cached answer, which is the larger
part. That saving does not justify leaving Google's 1024 for its 768, and
it would rule out bge-m3. The probe saw no loss at 1024 against 3072 on
`gemini-embedding-2`: the paraphrase scored 0.925 against 0.915.

## The local model

The probe ran bge-m3 in Hugging Face's
[text-embeddings-inference](https://huggingface.co/docs/text-embeddings-inference/en/supported_models)
CPU image (`cpu-1.9`), limited to 2 CPUs and 4 GB of memory to stand in for a
small VPS. TEI supports the candidates on CPU: XLM-RoBERTa (bge-m3), Gemma3
(EmbeddingGemma), Qwen3 (Qwen3-Embedding-0.6B and `voyage-4-nano`).

| Measure | bge-m3 on TEI, 2 CPUs |
| --- | --- |
| Download | 2.2 GB of fp32 ONNX weights |
| Default settings | **Killed for exceeding 4 GB** while warming up |
| `--max-batch-tokens 2048 --auto-truncate` | 2.0 GB resident, steady |
| One short sentence, 20 calls | p50 61 ms, max 95 ms |
| A 290-token message, 5 calls | p50 1000 ms |

The container died in its warm-up step, which runs a batch of
`--max-batch-tokens`. Capping that at 2048 fixed it, at the cost of truncating long inputs. Latency
grows with the message: a short question is ten times faster than a remote
call, but a 290-token one is as slow as Gemini, and it holds the two cores
while it runs.

Smaller models cost less. Ollama lists bge-m3 (567M parameters) at 1.2 GB on
disk ([library](https://ollama.com/library/bge-m3)) and EmbeddingGemma
(300M) at 622 MB ([library](https://ollama.com/library/embeddinggemma)).
EmbeddingGemma gives 768 dimensions, Matryoshka down to 128, and 2048 tokens
of input ([model card](https://huggingface.co/google/embeddinggemma-300m)),
so it misses the 1024 preference, and its weights are under the Gemma terms
rather than an open licence. Qwen3-Embedding-0.6B (Apache 2.0, 32 to 1024
dimensions) and `voyage-4-nano` (340M, Apache 2.0) are the open-licence
options at about the same size as bge-m3. Their memory was not measured; at
fp32 they should land near bge-m3's 2 GB, and roughly half that quantized.

**Not worth it, as things stand.** The spec does not fix the VPS's size, and
ticket 10 states it. On a 4 GB box running Postgres, Redis, the gateway (the
load test caps it at 256 MB) and an OpenTelemetry collector, 2 GB for one
embedding server is half the machine, and a long message ties up the CPU the
gateway needs. More to the point, the map decided that the workspace pays
for its embeddings through its own credential. A local model is the platform
paying, in RAM and CPU instead of tokens, for every workspace that turns the
cache on. Revisit it only if that decision changes, and then prefer a
quantized model in a family a remote vendor also serves (bge-m3 with
OpenRouter and Cloudflare, or `voyage-4-nano` with Voyage), so a busy local
server can fail over without starting a new cache.

## Recommendation

- **Default family `gemini-embedding-2`, 1024 dimensions,** on the
  workspace's Gemini credential, through the OpenAI-compatible endpoint. The
  cache key is `vec:{workspace}:gemini-embedding-2:1024`.
- **Fallback inside the family:** the workspace's OpenRouter credential with
  `google/gemini-embedding-2`. A 402 or 429 there is a skip, and exhausting
  the family is a cache miss.
- **Second family `bge-m3`, 1024 dimensions,** for a workspace whose only
  embedding-capable credential is OpenRouter. Cloudflare and a local model
  join this family only if they are ever added.
- **`text-embedding-3-small` at 1024 and `mistral-embed`** are worth listing
  for workspaces that hold a paid OpenAI or Mistral credential, since the
  workspace pays either way. They are not free.
- **No suitable credential, no semantic cache.** A workspace with only Groq,
  Cerebras, Anthropic, or Mistral without billing cannot turn the setting on,
  and the dashboard says why. The platform pays for no embedding, local or
  remote.
- **No new vendor for the semantic cache's launch.** Cloudflare is the
  strongest addition, a generous free allowance on open-weight families, but
  it brings a two-part credential. Voyage is the strongest for Anthropic-only
  workspaces. Both can follow on demand.
- **One adapter shape covers all of it.** Every endpoint above speaks OpenAI's
  `POST /embeddings` with `model`, `input` and `dimensions`: Gemini's
  compatible endpoint, OpenRouter, OpenAI, Mistral, Cloudflare and a local
  TEI server. Ticket 03 needs one client, configured by base URL, not one per
  vendor.

## Open for tickets 03 and 05

- **Threshold per family.** No paraphrase reached 0.95: 0.925 on
  `gemini-embedding-2`, 0.886 on bge-m3, 0.945 on `gemini-embedding-001` with
  a task type. The unrelated text scored 0.43 to 0.66. Ticket 05 should store
  a threshold per family and calibrate each on a labelled set of paraphrases
  and near-misses before enabling it, not reuse one number.
- **The task prefix.** `gemini-embedding-2` expects its instruction in the
  text. Google's guide gives a query prefix for search and says symmetric
  tasks such as classification share one prefix on both sides
  ([embeddings](https://aistudio.google.com/docs/embeddings)). A cache
  compares one question to another, which is symmetric, and EmbeddingGemma's
  card names the form for it, "task: sentence similarity | query:"
  ([model card](https://huggingface.co/google/embeddinggemma-300m)).
  Whatever prefix is chosen is part of the family: changing it changes every
  vector, so it belongs in the key or in a version beside it.
- **The timeout.** Remote embedding adds about 0.5 to 1 s to every
  exact-cache miss from the development machine. Ticket 03's short timeout
  should be measured from the VPS. Around 1 s keeps the tail bounded, and a
  timeout is a miss.
- **The Gemini free quota.** Unpublished. Should the embedding spend count
  against the same `free:*` buckets as chat, or against buckets of its own
  in `free_quota_catalog`?
- **Which credential pays when a workspace holds several.** The adapter
  needs an order. The simplest is the workspace's choice in ticket 04's
  setting: one family, and its credentials in order.
- **What is embedded.** The spec says the last user message. Two
  conversations that end in the same question but differ earlier would
  share an answer. Ticket 05 should decide whether the system prompt or the
  route belong in the text or the key.
