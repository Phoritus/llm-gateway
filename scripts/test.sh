#!/usr/bin/env bash
# Run the test suites.
#
# Usage: scripts/test.sh unit | int | race | all | db-up | db-down
#
# The tests that need Postgres and Redis skip themselves unless
# DATABASE_TEST_URL and REDIS_TEST_URL are set, so "unit" is simply the suite
# run without them.
#
# The integration suite gets a Postgres container of its own rather than the
# compose one, for two reasons: it truncates every table, which would wipe the
# development database, and a Postgres installed on the host shadows the
# compose container's published 5432 on some machines.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require go "Install Go 1.26 or newer."

# ENV_FILE records which host port the compose Redis publishes on, which may not
# be the default if something else already held it.
load_env || true
redis_port="${REDIS_PORT:-6379}"

test_db_url="postgres://gateway:gateway@127.0.0.1:${TEST_PG_PORT}/${TEST_PG_DB}?sslmode=disable"

# test_db_up starts the throwaway Postgres, reusing it when it is already
# running so a second test run does not pay for the startup.
test_db_up() {
	require_docker

	local state published
	state="$(docker inspect -f '{{.State.Status}}' "$TEST_PG_CONTAINER" 2>/dev/null || true)"

	# A container keeps the mapping it was created with, so changing
	# TEST_PG_PORT has no effect until the old one goes. Reusing it instead
	# fails as a bind error against the port you just stopped asking for,
	# which reads like the new port being the unavailable one.
	if [[ -n "$state" ]]; then
		published="$(docker inspect -f '{{(index (index .HostConfig.PortBindings "5432/tcp") 0).HostPort}}' "$TEST_PG_CONTAINER" 2>/dev/null || true)"
		if [[ -n "$published" && "$published" != "$TEST_PG_PORT" ]]; then
			log "the test database container publishes $published, not $TEST_PG_PORT: recreating it"
			test_db_down
			state=""
		fi
	fi

	case "$state" in
	running) ;;
	exited | created | paused)
		log "restarting the test database container"
		docker start "$TEST_PG_CONTAINER" >/dev/null
		;;
	*)
		log "creating the test database on port $TEST_PG_PORT"
		# A port can be unbindable with nothing listening on it, so port_in_use
		# cannot screen for this and docker's refusal is the only signal. On
		# Windows the Hyper-V dynamic ranges are excluded from binding -- netsh
		# int ipv4 show excludedportrange protocol=tcp -- and the default 55432
		# sits inside one of those blocks on some machines. Left unhandled, the
		# failure leaves a container in `created` that every later run then tries
		# to restart, so the next message blames a port nobody asked for.
		if ! docker run -d \
			--name "$TEST_PG_CONTAINER" \
			-e POSTGRES_USER=gateway \
			-e POSTGRES_PASSWORD=gateway \
			-e "POSTGRES_DB=$TEST_PG_DB" \
			-p "127.0.0.1:${TEST_PG_PORT}:5432" \
			"$TEST_PG_IMAGE" >/dev/null; then
			test_db_down
			die "could not publish the test database on 127.0.0.1:$TEST_PG_PORT. Pick a free port with TEST_PG_PORT, for example: TEST_PG_PORT=55032 make test-int"
		fi
		;;
	esac

	local deadline=$((SECONDS + 90))
	until docker exec "$TEST_PG_CONTAINER" pg_isready -U gateway -d "$TEST_PG_DB" >/dev/null 2>&1; do
		((SECONDS < deadline)) || die "the test database did not become ready within 90s"
		sleep 1
	done
	log "test database ready at 127.0.0.1:$TEST_PG_PORT"
}

test_db_down() {
	require_docker
	log "removing the test database container"
	docker rm -f "$TEST_PG_CONTAINER" >/dev/null 2>&1 || true
}

# test_redis_up brings up only the compose Redis. The tests use a separate
# logical database from development, and clean up by deleting their own keys:
# no test may FLUSHDB, because `go test ./...` runs packages in parallel
# against this one Redis and a flush in one lands mid-run in another.
test_redis_up() {
	require_docker
	compose up -d redis >/dev/null
	local deadline=$((SECONDS + 60))
	until compose exec -T redis redis-cli ping >/dev/null 2>&1; do
		((SECONDS < deadline)) || die "Redis did not become ready within 60s"
		sleep 1
	done
}

run_integration() {
	test_db_up
	test_redis_up

	export DATABASE_TEST_URL="$test_db_url"
	export REDIS_TEST_URL="redis://127.0.0.1:${redis_port}/${TEST_REDIS_DB}"

	log "running every test, including the ones that need Postgres and Redis"
	go test -count=1 "$@" ./...
}

case "${1:-unit}" in
unit)
	shift || true
	# Unset so an exported value in the caller's shell does not silently turn
	# this into an integration run.
	unset DATABASE_TEST_URL REDIS_TEST_URL
	log "running the unit tests (integration tests will skip)"
	go test -count=1 "$@" ./...
	;;
int | integration)
	shift || true
	run_integration "$@"
	;;
race)
	shift || true
	if ! command -v gcc >/dev/null 2>&1 && [[ "${CGO_ENABLED:-}" != "1" ]]; then
		die "the race detector needs cgo and a C compiler. Install one (for example: winget install BrechtSanders.WinLibs.POSIX.UCRT) or run 'make test-int' instead."
	fi
	export CGO_ENABLED=1
	run_integration -race "$@"
	;;
all)
	shift || true
	run_integration "$@"
	;;
db-up)
	test_db_up
	log "DATABASE_TEST_URL=$test_db_url"
	;;
db-down)
	test_db_down
	;;
*)
	die "unknown command '${1}'. Use unit, int, race, all, db-up or db-down."
	;;
esac
