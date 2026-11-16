#!/usr/bin/env bash
# Load the data plane against the mock provider and check the spec's targets:
# under 10ms added latency at p50, and memory well under Render's 512 MB.
#
# Usage: scripts/loadtest.sh       (Postgres and Redis up: make deps-up)
#
# It starts its own gateway on LOAD_PORT (default 18081). LOAD_RATE (requests a
# second, default 100) and LOAD_DURATION (default 30s) set each run; one run is
# non-streaming, one streaming. Each run creates a throwaway account in the dev
# database.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

VEGETA="github.com/tsenart/vegeta/v12@v12.13.0"
P50_LIMIT_MS=10
MEMORY_LIMIT_MB=256

require go "Install Go 1.26 or newer."
require uv "Install it from https://docs.astral.sh/uv/."
load_env || die "could not read $ENV_FILE"
[[ "${APP_ENV:-development}" == "development" ]] ||
	die "APP_ENV=$APP_ENV: the load test uses the mock provider, which only development serves"

port="${LOAD_PORT:-18081}"
rate="${LOAD_RATE:-100}"
duration="${LOAD_DURATION:-30s}"
base="http://127.0.0.1:$port"
work="$(mktemp -d)"
gateway_pid=""
cleanup() {
	[[ -n "$gateway_pid" ]] && kill "$gateway_pid" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT

log "building the gateway and vegeta"
go build -o bin/gateway ./cmd/gateway
GOBIN="$REPO_ROOT/bin" go install "$VEGETA"

log "starting a gateway on :$port at $rate requests a second for $duration"
PORT="$port" \
	OTEL_EXPORTER_OTLP_ENDPOINT="" \
	V1_KEY_RPM=10000000 V1_KEY_TPM=1000000000 \
	V1_WORKSPACE_RPM=10000000 V1_WORKSPACE_TPM=1000000000 \
	V1_WORKSPACE_CONCURRENCY=10000 \
	bin/gateway >"$work/gateway.log" 2>&1 &
gateway_pid=$!
for _ in $(seq 1 600); do
	curl -fsS "$base/healthz" >/dev/null 2>&1 && break
	kill -0 "$gateway_pid" 2>/dev/null || die "the gateway exited: $(tail -5 "$work/gateway.log")"
	sleep 0.5
done
curl -fsS "$base/healthz" >/dev/null || die "the gateway did not become healthy on :$port"

COMPAT_BASE_URL="$base" COMPAT_KEY_FILE="$work/key" uv run --quiet --script scripts/compat/compat.py --setup-only
key="$(cat "$work/key")"

targets() {
	local body
	body="$(printf '{"model":"compat-echo","stream":%s,"messages":[{"role":"user","content":"measure the gateway, not the model"}]}' "$1" | base64 | tr -d '\r\n')"
	printf '{"method":"POST","url":"%s/v1/chat/completions","header":{"Authorization":["Bearer %s"],"Content-Type":["application/json"]},"body":"%s"}\n' \
		"$base" "$key" "$body"
}

peak_memory_mb() {
	if [[ -r "/proc/$gateway_pid/winpid" ]]; then
		powershell.exe -NoProfile -Command "[int]((Get-Process -Id $(cat "/proc/$gateway_pid/winpid")).PeakWorkingSet64 / 1MB)" | tr -d '\r'
	elif [[ -r "/proc/$gateway_pid/status" ]]; then
		awk '/^VmHWM:/ { print int($2 / 1024) }' "/proc/$gateway_pid/status"
	fi
}

targets false | bin/vegeta attack -format=json -rate=20 -duration=5s >/dev/null

failed=0
for stream in false true; do
	targets "$stream" | bin/vegeta attack -format=json -rate="$rate" -duration="$duration" >"$work/results.bin"
	bin/vegeta report -type=json <"$work/results.bin" >"$work/report.json"
	label="non-streaming"
	[[ "$stream" == true ]] && label="streaming"
	printf '\n%s:\n' "$label"
	bin/vegeta report <"$work/results.bin" | sed -n '1,4p;/Status Codes/p'
	read -r p50_ms success < <(uv run --quiet --no-project python -c '
import json, sys
r = json.load(open(sys.argv[1]))
print(r["latencies"]["50th"] / 1e6, int(r["success"] == 1))
' "$work/report.json" | tr -d '\r')
	if awk -v p="$p50_ms" -v lim="$P50_LIMIT_MS" 'BEGIN { exit !(p < lim) }'; then
		log "$label p50 ${p50_ms}ms, under ${P50_LIMIT_MS}ms"
	else
		warn "$label p50 ${p50_ms}ms, not under ${P50_LIMIT_MS}ms"
		failed=1
	fi
	if [[ "$success" != 1 ]]; then
		warn "$label: not every request succeeded (success ratio $success)"
		failed=1
	fi
done

peak="$(peak_memory_mb)"
if [[ -z "$peak" ]]; then
	warn "cannot read the gateway's memory on this platform"
elif ((peak < MEMORY_LIMIT_MB)); then
	log "peak memory ${peak} MB, under ${MEMORY_LIMIT_MB} MB"
else
	warn "peak memory ${peak} MB, not under ${MEMORY_LIMIT_MB} MB"
	failed=1
fi

[[ "$failed" == 0 ]] || die "the load test missed a target"
log "every target met"
