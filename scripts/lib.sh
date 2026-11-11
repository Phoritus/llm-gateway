#!/usr/bin/env bash
# Shared helpers for the scripts in this directory.
#
# Every script here is runnable on its own, so the Makefile stays a thin list
# of names and nothing breaks for anyone without GNU make installed.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Container and port names used by the test fixtures. Overridable from the
# environment so two checkouts can run side by side.
TEST_PG_CONTAINER="${TEST_PG_CONTAINER:-llm-gateway-test-pg}"
TEST_PG_PORT="${TEST_PG_PORT:-55432}"
TEST_PG_IMAGE="${TEST_PG_IMAGE:-postgres:17-alpine}"
TEST_PG_DB="${TEST_PG_DB:-llm_gateway_test}"
TEST_REDIS_DB="${TEST_REDIS_DB:-1}"

# ENV_FILE is the one file local work reads: the service's variables for
# "make run" and the host ports compose publishes on. .env.prod is refused by
# name. It holds the live deployment's secrets and is not backed up anywhere,
# so no script here may read it, let alone regenerate its secrets.
ENV_FILE="${ENV_FILE:-.env.dev}"
if [[ "$(basename "$ENV_FILE")" == ".env.prod" ]]; then
	printf 'error: ENV_FILE=%s refused: the scripts never read the production env file
' "$ENV_FILE" >&2
	exit 1
fi

if [[ -t 1 ]]; then
	C_INFO=$'\033[1;34m'
	C_WARN=$'\033[1;33m'
	C_ERR=$'\033[1;31m'
	C_OFF=$'\033[0m'
else
	C_INFO='' C_WARN='' C_ERR='' C_OFF=''
fi

log() { printf '%s==>%s %s\n' "$C_INFO" "$C_OFF" "$*"; }
warn() { printf '%swarning:%s %s\n' "$C_WARN" "$C_OFF" "$*" >&2; }
die() {
	printf '%serror:%s %s\n' "$C_ERR" "$C_OFF" "$*" >&2
	exit 1
}

# require <command> <hint>
require() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is not on PATH. $2"
}

require_docker() {
	require docker "Install Docker Desktop, or start Postgres and Redis yourself."
	docker info >/dev/null 2>&1 ||
		die "the Docker engine is not responding. Start Docker Desktop and try again."
}

# random_base64 <bytes> prints that many random bytes, base64 encoded, with no
# trailing newline.
random_base64() {
	local bytes="$1"
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -base64 "$bytes" | tr -d '\r\n'
	else
		head -c "$bytes" /dev/urandom | base64 | tr -d '\r\n'
	fi
}

# load_env reads ENV_FILE into the environment.
#
# It parses KEY=VALUE itself rather than sourcing the file. Sourcing would
# execute whatever is in there, and a value containing spaces — which
# .env.example has — would be read as a command and fail.
load_env() {
	local file="${1:-$ENV_FILE}"
	[[ -f "$file" ]] || return 1

	# A key written twice is refused rather than resolved. The last assignment
	# would otherwise win in silence, which is how a leftover DATABASE_URL at
	# the bottom of the file sent a locally started service at a production
	# database: both lines looked right in isolation and nothing reported which
	# one took effect.
	local -A seen=()
	local -i lineno=0
	local line key value
	while IFS= read -r line || [[ -n "$line" ]]; do
		lineno+=1
		line="${line%$'\r'}"
		[[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
		[[ "$line" == *=* ]] || continue

		key="${line%%=*}"
		value="${line#*=}"
		key="${key//[[:space:]]/}"
		key="${key#export}"
		[[ -n "$key" ]] || continue

		if [[ -n "${seen[$key]:-}" ]]; then
			die "$file sets $key twice, on lines ${seen[$key]} and $lineno. Delete one: the later line would win and nothing would say so."
		fi
		seen[$key]=$lineno

		# Strip one layer of matching quotes, as dotenv readers do.
		if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then
			value="${value:1:${#value}-2}"
		fi

		export "$key=$value"
	done <"$file"
}

# port_in_use <port> reports whether anything is already listening locally.
#
# /dev/tcp rather than ss or netstat: it is built into bash and behaves the
# same in Git Bash on Windows as it does on Linux.
port_in_use() {
	local port="$1"
	if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
		exec 3<&- 2>/dev/null || true
		return 0
	fi
	return 1
}

# pick_port <preferred> [attempts] prints the first free port at or after the
# preferred one.
pick_port() {
	local port="$1" attempts="${2:-20}" i
	for ((i = 0; i < attempts; i++)); do
		if ! port_in_use "$port"; then
			printf '%s' "$port"
			return 0
		fi
		port=$((port + 1))
	done
	die "no free port found starting at $1"
}

# compose runs docker compose for this project. Compose only finds a file
# named .env on its own, so the port overrides in ENV_FILE are passed in.
compose() {
	if [[ -f "$ENV_FILE" ]]; then
		docker compose --env-file "$ENV_FILE" "$@"
	else
		docker compose "$@"
	fi
}
