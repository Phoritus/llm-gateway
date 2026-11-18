# 06 Context handoff: what it is

Type: grilling
Status: needs-triage
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
