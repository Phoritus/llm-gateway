# 07 Context handoff

Type: task
Status: ready-for-agent
Blocked by: 06

Build ticket 06's decided design
([research.md](../context-handoff/research.md#recommendation-for-this-gateway),
with the owner's decisions in ticket 06), behind the global `CONTEXT_HANDOFF`
switch:

- **Route setting** `context_handoff`: `off`, `reorder` or `trim`, default
  `reorder`, on `POST` and `PUT /api/routes` beside `cache` and `free_first`.
- **Windows:** learned from each vendor's context-too-long error (parse the
  limit it states) into `ctxwin:{credential}:{model}` in Redis with a TTL,
  numbers only; a window catalogue seeded from OpenRouter and LiteLLM data
  as the starting guess, a learned window always winning.
- **Reorder** targets whose known window fits first, the way
  `quota.Prioritize` reorders. Loses nothing.
- **Trim** (routes set to `trim`): the fitter from
  [the prototype](../context-handoff/prototype/), changed so only system and
  developer messages, the tools and the final turn are pinned. The first
  user message is no longer kept (owner's decision 7): drop its pin from the
  head and update the property test to match. One fitted retry of a target
  that answered context too long. No summarising call.
- **Visibility:** `X-Context-Handoff` with values from a fixed set, the
  action on the fallback trail and the usage event, a
  `gateway.context_handoff` metric by action, and an answer to a fitted
  request never cached under the original request's key.
- **Errors:** exhaustion where every failure was a context error answers 400
  `context_length_exceeded` with the learned limit in the message, not 413.
  Change the spec's honest-exhaustion line and the CLAUDE.md invariant in the
  same PR, and remove the spec's `sess:` row from its Redis table.
- **Proof:** a window-aware mock model (`mock/window-<N>`) and a router test:
  a long conversation over an 8k target and a rate-limited 128k one fails
  today and is answered with `trim` on.

## Comments
