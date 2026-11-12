# 18 Load test against the mock: under 10ms p50 added latency

Status: ready-for-agent

The spec's goal is added latency under 10 ms at p50 against the mock, and
bounded memory under load. Use k6 or vegeta. If it becomes a repeatable step,
add a `make` target (CLAUDE.md: every command is a make target). This can run
now against `mock/echo` in development. Spec: [Goals](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md), [Testing](../../../Backend%20Architecture%20BYOK%20LLM%20Gateway%20(Go).md).

## Comments
