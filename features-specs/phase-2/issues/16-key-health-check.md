# 16 Periodic provider key health check

Type: task
Status: resolved
Blocked by: -

The spec's optional background job: test each credential now and then with
the adapter's `CheckKey` (it generates nothing) and record the result and
time on the credential, shown by `GET /api/credentials`, so a revoked key is
visible before a request has to fail over it. Spread the checks out, at most
one per credential a day, and never let a check move the router's skip
state: that stays driven by real traffic.

## Answer

`internal/keycheck` runs in every instance, on the same background context
as the other sweepers. Once a minute it claims up to 5 due credentials and
checks each through the adapter's `CheckKey`, under a 10s deadline.

- **Spreading.** Each credential carries `check_due_at`, `now()` on a new
  row. `store.Credentials.ClaimDueChecks` pushes the claimed rows a day
  ahead in the same statement, with `FOR UPDATE SKIP LOCKED`. Two instances
  never check one key, and a crash mid-check delays that key by a day
  instead of retrying it in a loop. The 5-a-minute claim bounds the calls
  one instance makes to all providers together.
- **Results.** `check_result` holds one of `ok`, `key_rejected`,
  `quota_exhausted`, `rate_limited`, `provider_timeout`, `provider_error` or
  `provider_unavailable` (a CHECK constraint), with `checked_at`. These are
  the codes `POST /api/credentials/{id}/test` already answered with, so the
  manual test and the background check share `keycheck.Result`, and the
  manual test records its result too.
- **The API.** `GET /api/credentials` shows `last_check`, either null or
  `{result, checked_at}`.
- **Router state.** The checker calls the adapter, never `router`, so skips,
  the breaker and the bench stay driven by real traffic only.

## Comments
