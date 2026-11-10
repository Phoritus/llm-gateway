#!/usr/bin/env bash
# Manage the local Postgres and Redis from docker-compose.yml.
#
# Usage: scripts/deps.sh up | down | reset | status | wait | psql | redis-cli

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# wait_healthy blocks until every compose service reports healthy, so that a
# following "make run" does not race the database's first startup.
wait_healthy() {
	local deadline=$((SECONDS + 120))
	local names status all_ok

	log "waiting for Postgres and Redis to report healthy"
	while :; do
		all_ok=1
		names="$(compose ps --services 2>/dev/null || true)"
		[[ -n "$names" ]] || all_ok=0

		while IFS= read -r service; do
			[[ -n "$service" ]] || continue
			status="$(compose ps --format '{{.Health}}' "$service" 2>/dev/null | head -1)"
			# An empty health means the container is not up yet. A service
			# without a healthcheck would report "" forever, but both of ours
			# define one.
			[[ "$status" == "healthy" ]] || all_ok=0
		done <<<"$names"

		if [[ "$all_ok" -eq 1 ]]; then
			log "dependencies are ready"
			return 0
		fi
		if ((SECONDS >= deadline)); then
			compose ps
			die "dependencies did not become healthy within 120s"
		fi
		sleep 2
	done
}

case "${1:-up}" in
up)
	require_docker
	log "starting Postgres and Redis"
	compose up -d
	wait_healthy
	# A Postgres installed on the host claims 5432 first and the published
	# container port is then unreachable, which shows up as a password
	# failure rather than a connection error. Worth saying once, here.
	log "if connecting to 127.0.0.1:5432 fails to authenticate, another Postgres already owns that port"
	;;
down)
	require_docker
	log "stopping Postgres and Redis, keeping their data"
	compose down
	;;
reset)
	require_docker
	warn "this deletes the local Postgres and Redis volumes"
	compose down -v
	log "volumes dropped; the next 'up' starts from an empty database"
	;;
status)
	require_docker
	compose ps
	;;
wait)
	require_docker
	wait_healthy
	;;
psql)
	require_docker
	compose exec postgres psql -U gateway -d llm_gateway
	;;
redis-cli)
	require_docker
	compose exec redis redis-cli
	;;
*)
	die "unknown command '${1}'. Use up, down, reset, status, wait, psql or redis-cli."
	;;
esac
