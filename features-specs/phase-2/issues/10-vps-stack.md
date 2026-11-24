# 10 Finish the VM stack: memory limits, a deploy that does not cut requests

Type: task
Status: resolved
Blocked by: -

Production is one Azure VM running `deploy/compose.yml`: the gateway and
Caddy, behind Cloudflare, shipped by `make deploy`. Postgres is Azure's
managed Postgres and Redis is Upstash, both outside the VM. Azure backs
Postgres up, so this ticket writes no `pg_dump` job. What is left:

- Memory limits per container, and a statement of how much the VM needs.
- A deploy that restarts without cutting requests. `make deploy` runs
  `docker compose up -d --build` today, which drops the open connections
  for as long as the restart takes. R12's shutdown budget applies.
- The `redis` service in compose stays, for the semantic cache's vectors
  only (ticket 05). Give it a memory limit like the rest, and keep it off
  the public ports.

No secret goes in a committed file. The VM reads `deploy/.env.prod`, which the
scripts refuse to open locally.

## Comments

- 2026-11-18: ticket 01: run the stock `redis:8-alpine`, pinned to a minor version (the prototype ran 8.10.3), unmodified, since the AGPLv3 duty is triggered only by patching it. See [research.md](../vector-search/research.md).
- 2026-11-23: rescoped to the owner's hosting. The compose file, Caddy and `make deploy` are done (PRs #35 to #37). There is no Postgres on the box, so this ticket has no backups or restore script, and Redis is not on the box either.

## Answer

- `deploy/compose.yml`: two gateways (`gateway-a`, `gateway-b`, one image)
  at 256 MiB each with `GOMEMLIMIT=220MiB`, Redis at 192 MiB (128 MiB of
  data, pinned to `redis:8.10-alpine`, no ports), Caddy at 128 MiB: 832 MiB
  at most. A Go build on the VM wants about 1 GiB more, so the VM needs 2 GiB.
- A gateway that gets SIGTERM answers `/healthz` with 503 for 3s
  (`deregisterDelay`, at most half the drain window) before it stops
  accepting, then drains inside `SHUTDOWN_TIMEOUT`. `stop_grace_period` is
  35s, past the 25s default. `/healthz`, not `/readyz`, is what Caddy polls,
  so a Postgres or Upstash blip does not take every instance out.
- Caddy balances the two with a 1s health check on `/healthz` and waits up to
  30s for a gateway when neither answers.
- `deploy/roll.sh`, run by `make deploy`: build once, restart one gateway at
  a time and wait for it, reload Caddy (never recreate it: a recreated Caddy
  refuses connections while it is down, 443 dropped in the rehearsal), and on the first deploy stop the old single `gateway` gracefully
  before removing it.
- Proof, rehearsed locally with the real compose stack and 8 threads of POSTs:
  a steady-state deploy dropped 0 of about 90,000 requests (and 0 of 123,000
  before that). The first deploy, from the old single-gateway stack, dropped
  1 at the instant Caddy reloaded its config; Caddy skips an unchanged config,
  so later deploys do not reload.
- Caddy has no `flush_interval`: SSE is flushed at once without it, and `-1`
  would stop Caddy cancelling the upstream when a client disconnects.
