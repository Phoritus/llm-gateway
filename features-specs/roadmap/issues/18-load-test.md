# 18 Load test against the mock: under 10ms p50 added latency

Status: resolved

The spec's goal is added latency under 10 ms at p50 against the mock, and
bounded memory under load. Use k6 or vegeta. If it becomes a repeatable step,
add a `make` target (CLAUDE.md: every command is a make target). This can run
now against `mock/echo` in development. Spec: [Goals](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Testing](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments

- 2026-11-16, resolved: `make loadtest` (`scripts/loadtest.sh`) starts its own
  gateway on `LOAD_PORT` with the `/v1` limits raised and telemetry export
  off, creates a workspace through `compat.py --setup-only`, and drives
  `compat-echo` (`mock/echo`, no delay of its own, cache off) with vegeta
  v12.13.0, non-streaming then streaming. It fails unless p50 is under 10ms,
  every request succeeds and peak memory is under 256 MB (Windows peak
  working set or Linux VmHWM). Measured on the Windows dev machine, Postgres
  and Redis in Docker:

  | Run | p50 | p95 | p99 | max |
  | --- | --- | --- | --- | --- |
  | 100/s 30s, non-streaming | 1.59ms | 2.20ms | 2.68ms | 6.8ms |
  | 100/s 30s, streaming | 1.64ms | 2.16ms | 2.64ms | 5.7ms |
  | 500/s 60s, non-streaming | 2.58ms | 6.04ms | 16.4ms | 148ms |
  | 500/s 60s, streaming | 3.21ms | 12.2ms | 319ms | 614ms |

  Every request answered 200. Memory: 83 MB working set idle, a peak of
  155 MB at 100/s and 151 MB at 500/s, so it does not grow with load. The
  p50 target holds with a wide margin. The 500/s streaming tail (p99 319ms)
  was not investigated: it is no target, and a single dev machine running the
  load generator, the gateway and both databases is not where to measure it.
