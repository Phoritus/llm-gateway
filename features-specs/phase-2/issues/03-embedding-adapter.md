# 03 Embedding adapter

Type: task
Status: ready-for-agent
Blocked by: 02

One interface with interchangeable clients, as the spec says, for the
vendors ticket 02 picks: embed a text with a workspace credential, and report
the model family and dimensions with the vector. Typed errors like the chat
adapters, a short timeout, and failover only inside one family. A failure or
timeout is never an error to the client: the semantic cache treats it as a
miss. A mock embedder for tests, the way `provider/mock` serves chat.

The spec's code layout puts embedding adapters in `cache/`.

## Comments

- 2026-11-18: ticket 02 answered which embedders: see [research.md](../embeddings/research.md) and its open questions for this ticket.
