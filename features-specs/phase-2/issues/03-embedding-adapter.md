# 03 Embedding adapter

Type: task
Status: resolved
Blocked by: 02

One interface with interchangeable clients, as the spec says, for the
vendors ticket 02 picks: embed a text with a workspace credential, and report
the model family and dimensions with the vector. Typed errors like the chat
adapters, a short timeout, and failover only inside one family. A failure or
timeout is never an error to the client: the semantic cache treats it as a
miss. A mock embedder for tests, the way `provider/mock` serves chat.

The spec's code layout puts embedding adapters in `cache/`.

## Answer

`provider.Embedder` is its own interface, since Anthropic has none;
`provider/openai` serves it over `POST /embeddings` with the chat path's typed
errors, and the mock returns a bag-of-words vector. `cache.Families` is the
table of families, `cache.Embeddings.Embed` the failover: targets in the
workspace's order, those the family does not name skipped, one deadline over
the whole call, a wrong-sized or zero vector a failure of that target, every
answer normalised. Not wired into `/v1`: ticket 05 does that and picks the
deadline (about 1s). Checked live: Gemini answered 1024 dimensions in 620 ms,
a bad Gemini key failed over to OpenRouter, and the two vectors had cosine
1.00000. The task prefix stays open for ticket 05; it would be part of a
family's definition.

## Comments

- 2026-11-18: ticket 02 answered which embedders: see [research.md](../embeddings/research.md) and its open questions for this ticket.
