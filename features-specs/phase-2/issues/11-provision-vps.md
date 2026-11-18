# 11 Provision the VPS, DNS and secrets

Type: task
Status: ready-for-human
Blocked by: 10

By hand: choose and pay for a VPS (ticket 10 says how much memory the stack
needs), create an SSH key for deploys, point DNS for the service's hostname
at it, create the off-machine backup bucket, and write the production env
file on the server. `MASTER_KEY` is copied from the current deployment, never
regenerated, or every stored provider key stops opening.

## Comments
