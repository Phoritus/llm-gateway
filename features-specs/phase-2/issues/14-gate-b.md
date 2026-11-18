# 14 Gate B: go live

Type: task
Status: ready-for-human
Blocked by: 05, 07, 09, 13, R16

The go-live check on the VPS: `make loadtest` against the new stack meets the
spec's targets (p50 added latency under 10ms), a backup is restored into a
scratch database, one alert is made to fire and its mail arrives,
`make compat` and `make live` pass against production, and the real-mail
check (R16) is done. The reset-password page belongs to the frontend repo,
not to this gate.

## Comments
