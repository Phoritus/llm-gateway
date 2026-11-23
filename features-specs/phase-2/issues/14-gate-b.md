# 14 Gate B: go live

Type: task
Status: ready-for-human
Blocked by: 05, 07, 09, 10, 12, 13, R16

The go-live check on the VM:

- The live hostname answers `/readyz` over HTTPS through Cloudflare.
- `make loadtest` meets the spec's target (p50 added latency under 10ms).
- One Grafana Cloud alert is made to fire, and its mail arrives.
- `make compat` and `make live` pass against production.
- The real-mail check (R16) is done.
- An Azure point-in-time restore of Postgres has been tried once, into a
  scratch server.

The reset-password page belongs to the frontend repo, not to this gate.

## Comments
