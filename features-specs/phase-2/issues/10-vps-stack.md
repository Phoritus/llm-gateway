# 10 The VPS stack: compose, Caddy, backups, deploy

Type: task
Status: ready-for-agent
Blocked by: 01

Phase 2 consolidates onto one VPS. Write the production stack as files and
`make` targets, as the rest of the repo does: a compose file with the
gateway, Postgres, Redis (the image ticket 01 picks), Caddy for TLS and an
OpenTelemetry collector; a nightly `pg_dump` copied off the machine, and a
restore script that has been run once; and a deploy target that builds the
image, ships it and restarts without cutting requests (R12's shutdown budget
applies). Memory limits per container, and a statement of how much the
machine needs for ticket 11. No secret in a committed file: the server reads
its own env file, which the scripts refuse to open locally, as with
`.env.prod` today.

## Comments

- 2026-11-18: ticket 01: run the stock `redis:8-alpine`, pinned to a minor version (the prototype ran 8.10.3), unmodified, since the AGPLv3 duty is triggered only by patching it. See [research.md](../vector-search/research.md).
