# 11 Provision the VM, DNS and secrets

Type: task
Status: resolved
Blocked by: -

By hand: create the VM and an SSH key for deploys, point DNS for the
service's hostname at it, and write the production env file on the server.
`MASTER_KEY` is copied from the current deployment, never regenerated, or
every stored provider key stops opening.

## Answer

Done by the owner by 2026-11-23: an Azure VM (`azureuser`, key in `VM_KEY`),
Cloudflare in front, `DOMAIN` in `deploy/.env` and the service's settings in
`deploy/.env.prod`. Postgres is Azure's managed Postgres and Redis is Upstash,
so there is no backup bucket to create: Azure backs Postgres up. Gate B (14)
checks that the live hostname answers `/readyz` over HTTPS.

## Comments
