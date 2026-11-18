# 06 Context handoff: what it is

Type: grilling
Status: resolved
Blocked by: -

The spec names context handoff only as a Phase 2 feature, opt-in per route,
with one Redis key: `sess:{workspace}:{session}`, the last model and recent
messages, kept 3 hours. It does not say what problem it solves, and an OpenAI
chat request already carries the whole conversation. Questions for the user,
before any code:

- What goes wrong today that handoff fixes? For example, a conversation that
  fails over to a model with a smaller context window, or a client that sends
  only the newest message.
- What identifies a session: a header, the `user` field, or something the
  gateway mints?
- What the gateway does with the stored messages: prepend them, summarise
  them for a smaller model, or keep a session on its last model (sticky)
  instead of the route's first target.
- Prompt text is not stored unless the user opts in (spec, Data model), so
  storing recent messages has to be an opt-in with a stated retention.

The answers become ticket 07's scope.

## Comments

- 2026-11-18, the owner: handoff is for an IDE or any app whose conversation
  outgrows a model's context window, or whose big-window model is rate
  limited: the gateway should hand the conversation to another target in a
  form that fits, so the client keeps working unchanged. Research the best
  real-world approach, improve on it, reuse open source where it fits, and
  prove it.
- 2026-11-18, the owner decided:
  1. Trimming announced in `X-Context-Handoff` is enough; the route opt-in
     suffices and clients need not opt in.
  2. Reorder-by-window is on for every route by default; trimming is opt-in.
     Per-route `context_handoff: off | reorder | trim`, default `reorder`,
     under the global `CONTEXT_HANDOFF` switch.
  3. No summarising model call for now.
  4. Drop the spec's `sess:{workspace}:{session}` key: handoff is stateless.
  5. Context exhaustion answers 400 `context_length_exceeded` (OpenAI's
     shape), not 413, with the learned limit in the message.
  6. Windows: learned from vendor errors first, a catalogue seeded from
     OpenRouter and LiteLLM data as the starting guess.
  7. Rejected: the first user message is not pinned; it may be dropped like
     any other middle message.

## Answer

Research with sources: [research.md](../context-handoff/research.md). Proof:
its last section and [prototype/](../context-handoff/prototype/).

What breaks today is failover, not session memory: a context-too-long error
only skips the same model, so a long conversation fails when every remaining
target has a smaller window. Chat Completions sends the whole conversation
every turn, so handoff can be stateless, and the spec's `sess:` key (stored
prompt text) is not needed.

Design, per target, inside the router's pre-first-byte loop:

1. Reorder targets so those whose known window fits go first. Loses nothing.
2. On routes that opt in, fit the conversation to the chosen target: keep the
   system and developer messages, the tools and the final turn; clear
   old tool results (keeping the newest 3); drop whole middle units, never
   splitting a tool call from its results, behind one marker message. If what
   must be kept does not fit, the client gets the existing context error.
3. Summarising the dropped middle with another model call: left out for now.

Triggered before the call when the target's window is known, and on a
context-too-long error by retrying that target once, fitted to the limit the
error states. Windows are learned from those errors and kept in Redis as
numbers only. Every changed request is announced (`X-Context-Handoff`, the
fallback trail, the usage event), and an answer to a fitted request is never
cached under the original request's key.

Rejected: LiteLLM's `trim_messages` (leaves gaps, breaks tool pairs, drops
the shortened system message), LLMLingua (Python and a model), vendor
compaction (one vendor each, needs a window that already fits) and dropping
the oldest messages (orphans tool results, loses the task: see the proof).

The owner's answers to research.md's open questions are above.
