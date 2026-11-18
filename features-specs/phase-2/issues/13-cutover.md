# 13 Cut over from Render, Neon and Upstash

Type: task
Status: ready-for-human
Blocked by: 11, 12

An agent writes the runbook and rehearses it against a copy; the user runs
it. Postgres moves by dump and restore during a short write freeze. Redis
state is disposable (buckets, skips and the cache refill on their own) and is
not moved. Lower the DNS TTL beforehand, switch, watch the dashboards from 12,
and keep the Render service able to take traffic back until the VPS has run a
day. Check `PUBLIC_BASE_URL` (the mail links) and `ALLOWED_ORIGINS` on the
new host: R16 found a wrong `PUBLIC_BASE_URL` in production.

## Comments
