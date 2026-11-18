# 12 Prometheus, Grafana and alerts on the VPS

Type: task
Status: ready-for-agent
Blocked by: 10

Phase 2 moves metrics from Grafana Cloud to Prometheus and Grafana on the
VPS. The service already exports OTLP, so the collector from ticket 10 feeds
Prometheus and no instrumentation changes. Provision, as committed files: the
data source, a dashboard (requests by vendor and outcome, time to first
token, fallback attempts, cache hits, rate-limit rejections, breaker
openings, upstream errors, usage drops) and the spec's starting alerts (error
rate, p95 latency, Postgres or Redis down, usage queue nearly full) mailed
through the SMTP relay. Grafana is not public: behind Caddy with its own
login, or reachable over SSH only. `make obs-up` already runs a local stack
to try it against.

Spec: [Observability and background jobs](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
