#!/usr/bin/env bash
# Check that real clients work against a running development gateway: the
# official OpenAI Python SDK, then the Continue CLI, which runs the same core
# and model config as the Continue IDE extensions.
#
# Usage: scripts/compat.sh            (start the gateway first: make run)
#
# Everything runs against the mock provider, so no provider quota is spent.
# Each run creates a throwaway account in the development database.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

CONTINUE_CLI="@continuedev/cli@1.5.47"

require uv "Install it from https://docs.astral.sh/uv/."
require npx "Install Node.js 20 or newer."
load_env || die "could not read $ENV_FILE"
[[ "${APP_ENV:-development}" == "development" ]] ||
	die "APP_ENV=$APP_ENV: the compatibility check uses the mock provider, which only development serves"

base="http://127.0.0.1:${PORT:-8080}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

log "OpenAI Python SDK"
COMPAT_BASE_URL="$base" COMPAT_KEY_FILE="$work/key" uv run --quiet --script scripts/compat/compat.py

log "Continue CLI ($CONTINUE_CLI)"
key="$(cat "$work/key")"
failed=0

cn_case() {
	local name=$1 model=$2 gateway_key=$3 want_exit=$4 want_out=$5
	cat >"$work/config.yaml" <<EOF
name: llm-gateway compat
version: 0.0.1
schema: v1
models:
  - name: gateway
    provider: openai
    model: $model
    apiBase: $base/v1
    apiKey: $gateway_key
    roles: [chat]
EOF
	local status=0
	CONTINUE_GLOBAL_DIR="$work/continue" npx --yes "$CONTINUE_CLI" \
		-p "hello from continue" --config "$work/config.yaml" --readonly >"$work/out" 2>&1 || status=$?
	if [[ "$status" == "$want_exit" ]] && grep -qF -- "$want_out" "$work/out"; then
		printf 'ok    %s\n' "$name"
	else
		printf 'FAIL  %s: exit %s, output: %s\n' "$name" "$status" "$(tail -c 400 "$work/out")"
		failed=1
	fi
}

cn_case "streamed answer" compat-echo "$key" 0 "hello from continue"
cn_case "upstream failure" compat-fail "$key" 1 "502 The upstream provider failed to answer."
cn_case "failure after the first chunk" compat-break "$key" 1 "The upstream provider failed to answer."
cn_case "bad key" compat-echo "gwk_$(printf 'A%.0s' {1..43})" 1 "401 The gateway key is invalid"

[[ "$failed" == 0 ]] || die "the Continue CLI check failed"
log "every client check passed"
