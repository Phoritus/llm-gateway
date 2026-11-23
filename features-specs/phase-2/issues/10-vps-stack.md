# 10 Finish the VM stack: memory limits, a deploy that does not cut requests

Type: task
Status: ready-for-agent
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
