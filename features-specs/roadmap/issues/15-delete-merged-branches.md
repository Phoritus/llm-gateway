# 15 Delete merged branches

Status: resolved
Blocked by: 01

The local `feat/telemetry-otel`, the merged feature branches on origin, and
`feat/mock-provider-v1` once PR #4 merges. Deleting remote branches is
outward-facing: an agent does it only after the user says go.

## Comments
Done 2026-11-12: `feat/mock-provider-v1` deleted locally and on origin; the other merged branches were already gone. Deleting merged branches is now a standing step (map.md, step 0), so no new ticket is needed for it.
