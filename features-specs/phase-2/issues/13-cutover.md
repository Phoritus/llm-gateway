# 13 Move Postgres from Neon to Azure

Type: task
Status: ready-for-human
Blocked by: -

Postgres moves from Neon to Azure's managed Postgres. Upstash stays as the
Redis, and the VM already serves traffic, so this is the only move left. An
agent writes the runbook and rehearses it against a copy; the owner runs it.

- Dump Neon and restore into Azure during a short write freeze. Goose's
  version table moves with the data, so the gateway does not re-run its
  migrations.
- Point `DATABASE_URL` in `deploy/.env.prod` at Azure (with
  `sslmode=require`), `make deploy`, and check `/readyz`.
- Keep Neon untouched until the VM has run a day on Azure.
- `MASTER_KEY` does not change: the stored provider keys are sealed under it.

If Neon holds nothing worth keeping, this ticket is only the second step.

## Comments

- 2026-11-23: was "Cut over from Render, Neon and Upstash". Render was never used (production went straight to the VM) and Upstash stays, so only Postgres moves.
