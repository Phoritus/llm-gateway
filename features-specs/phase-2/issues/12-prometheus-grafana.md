# 12 Dashboards and alerts on Grafana Cloud

Type: task
Status: ready-for-agent
Blocked by: -

Grafana Cloud stays the observability stack. The owner decided against a
self-hosted Prometheus and Grafana on 2026-11-23. The service already exports
OTLP to it, so no instrumentation changes. Commit, as files, and apply them to
Grafana Cloud:

- A dashboard: requests by vendor and outcome, time to first token, fallback
  attempts, cache hits, rate-limit rejections, breaker openings, upstream
  errors and usage drops.
- The spec's starting alerts: error rate, p95 latency, Postgres or Redis
  down, and the usage queue nearly full. They go through Grafana Cloud's own
  contact points (email).

Every label stays inside the free tier's roughly 10k active series (see
CLAUDE.md on `http.route`). `make obs-up` runs the same Grafana locally to
build the dashboard against.

Spec: [Observability and background jobs](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

- 2026-11-23: was "Prometheus, Grafana and alerts on the VPS". Rescoped to Grafana Cloud by the owner's decision; no longer blocked by 10.
