# 12 Dashboards and alerts on Grafana Cloud

Type: task
Status: resolved
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

## Answer

- `deploy/grafana/dashboard.json`: the LLM Gateway dashboard (uid
  `llm-gateway`), with its Prometheus as a variable so the same file works
  on Grafana Cloud and locally. Requests by vendor and outcome, time to first
  token, request duration, fallback attempts, upstream errors, breaker
  openings, cache lookups, rate-limit rejections, context handoff, usage drops
  and queue depth, the Postgres pool and HTTP by route. Every series is
  labelled by values from fixed sets only.
- `deploy/grafana/alerts.json`: upstream error rate above 5% (only above 3
  requests a minute), p95 time to first token above 10s, Postgres or Redis
  unreachable, the usage queue past 8000 (80% of the default
  `USAGE_QUEUE_SIZE`), usage events dropped, and no metrics for 5 minutes.
  They notify through the default notification policy, so the email contact
  point is set in Grafana Cloud.
- `make grafana` applies both, idempotently.
- Proof, against the local Grafana from `make obs-up`, fed by the real
  gateway and `make compat`: the metric names are the ones Prometheus stored
  (counters `_total`, seconds histograms `_seconds`), every panel query runs,
  and every alert rule evaluates with health `ok`; the error-rate rule went
  pending on compat's deliberate failures. The four counters compat does not
  trigger follow the same naming, and `ratelimit_rejections_total` was
  checked by triggering it.
- Fixed on the way: `make compat` still expected 413 for a context error,
  which ticket 07 changed to 400.
