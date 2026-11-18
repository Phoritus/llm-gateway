# 07 Context handoff

Type: task
Status: needs-triage
Blocked by: 06

Build ticket 06's design, behind `CONTEXT_HANDOFF` and a per-route
`context_handoff` setting (`off`, `reorder`, `trim`), as
[research.md](../context-handoff/research.md#recommendation-for-this-gateway)
recommends and the owner's answers to its open questions adjust:

- Learned windows: parse the limit from each vendor's context-too-long error,
  store `ctxwin:{credential}:{model}` in Redis with a TTL (numbers only).
- Reorder fitting targets first, the way `quota.Prioritize` reorders.
- The fitter, from [the prototype](../context-handoff/prototype/), with its
  property test, and one fitted retry of a target that answered context too
  long.
- `X-Context-Handoff` with values from a fixed set, the action on the trail
  and the usage event, a `gateway.context_handoff` metric by action, and no
  caching of a fitted request's answer under the original key.
- A window-aware mock model (`mock/window-<N>`) and a router test: a long
  conversation over an 8k target and a rate-limited 128k one fails today and
  is answered with handoff on.

## Comments
