# 21 Per-route first-byte timeout

Status: ready-for-agent
Blocked by: 11

Every adapter waits a fixed 30s for its first event
(`openai.Options.FirstByteTimeout`, `anthropic.Options.FirstByteTimeout`).
A reasoning model can think longer than that before its first chunk, and is
then answered `ErrTimeout` and failed over from. Make the wait a route
setting (or a target setting), with 30s as the default.

- A nullable column on `routes` (or `route_targets`), a bounded field on
  `POST /api/routes` and `PUT /api/routes/{id}`, returned by `ResolveRoute`.
- The adapters take the value per call rather than at construction, since one
  adapter serves every workspace.
- The router's 45s budget counts from the first attempt, so a long wait on the
  first target still leaves its first retry (always allowed) but may use up
  the rest. Decide whether the budget grows with the setting.

Deferred from ticket 11, where it was a comment from ticket 08's live run.

## Comments

- 2026-11-15, checking ticket 13's catalog with a free key: Gemini's
  `gemini-3.8-flash` streamed its first byte after 92s and 106s on a
  20-token "Say ok" (`gemini-3.5-flash-lite`: 0.9s). Through the gateway it
  can only end in `ErrTimeout`. It was left out of `free_quota_catalog` for
  that reason; a per-route timeout would let a route opt into it.
