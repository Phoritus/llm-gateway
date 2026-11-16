# 21 Per-route first-byte timeout

Status: resolved
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

- 2026-11-16, resolved, as a target setting rather than a route one: the
  thinker is one model in a chain, and the targets behind it should still be
  given up on after 30s. `route_targets.first_byte_timeout_seconds` (migration
  00008), nullable, 1 to 300; `POST /api/routes` and `PUT /api/routes/{id}`
  take it per target as `first_byte_timeout_seconds` and list it back, null
  for the default. `ResolveRoute` carries it as
  `ResolvedTarget.FirstByteTimeout`; `router.FirstByteTimeout(t)` applies the
  30s default. The timer moved out of both adapters into the gateway's
  `attempt`, which already spans opening the stream and reading its first
  chunk, so one place owns it and the mock is held to it too. The adapters
  now only have to give up promptly when their context ends, which their
  tests check. Anthropic's `message_start` still yields the first chunk (the
  role), so a Claude model that thinks after it is in time, as before. The
  budget grows by each attempted target's wait above the default, so opting
  one target into 120s leaves the targets after it their usual time.
