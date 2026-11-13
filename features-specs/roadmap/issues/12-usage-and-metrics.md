# 12 Usage events, rollups and metrics (Gate A: free launch)

Status: resolved
Blocked by: 11

Build-order step 6.

- A buffered channel and a background writer that batches into
  `usage_events`. It never delays a response; on overflow it drops the oldest
  and counts the drops.
- The `usage_daily` rollup, a retention job, and `GET /api/usage`.
- The spec's metrics (latency, time to first token, hops, rejections, breaker
  state, upstream errors by class), with bounded labels only.
- Graceful shutdown flushes the queue. `ShutdownTimeout` (20s) is far shorter
  than `V1_REQUEST_TIMEOUT` (5m), so long streams are cut on deploy; decide
  whether that is acceptable.

Spec: [Observability and background jobs](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
