#!/usr/bin/env bash
# Manage the local observability stack (Grafana, Prometheus, Tempo, Loki behind
# an OTLP receiver) from docker-compose.yml.
#
# Usage: scripts/obs.sh up | down | status
#
# The stack is a compose profile, so scripts/deps.sh never starts or waits on
# it, and "deps.sh down" leaves it alone. Only this script names the profile.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SERVICE=lgtm

obs_compose() {
	compose --profile observability "$@"
}

# env_value <key> <default> reads one variable from the environment, then from
# ENV_FILE, without loading the whole file: compose reads the same file for the
# port substitution, and this must agree with it.
env_value() {
	local key="$1" fallback="$2" value="${!1:-}"
	if [[ -z "$value" && -f "$ENV_FILE" ]]; then
		value="$(sed -n "s/^[[:space:]]*${key}=//p" "$ENV_FILE" | tail -1 | tr -d '\r')"
	fi
	printf '%s' "${value:-$fallback}"
}

wait_grafana() {
	require curl "It is used to poll Grafana's health endpoint."
	local port deadline=$((SECONDS + 120))
	port="$(env_value GRAFANA_PORT 3000)"

	log "waiting for Grafana on http://127.0.0.1:${port}"
	until curl -fsS --max-time 3 "http://127.0.0.1:${port}/api/health" >/dev/null 2>&1; do
		if ((SECONDS >= deadline)); then
			obs_compose ps
			die "Grafana did not answer /api/health within 120s. If the port is taken, set GRAFANA_PORT in $ENV_FILE."
		fi
		sleep 2
	done
	log "Grafana is up at http://127.0.0.1:${port} (admin / admin)"
	log "point the service at it with OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:$(env_value OTLP_HTTP_PORT 4318)"
}

case "${1:-status}" in
up)
	require_docker
	log "starting the observability stack"
	obs_compose up -d "$SERVICE"
	wait_grafana
	;;
down)
	require_docker
	log "stopping the observability stack"
	obs_compose rm -sf "$SERVICE"
	;;
status)
	require_docker
	obs_compose ps "$SERVICE"
	;;
*)
	die "unknown command '${1}'. Use up, down or status."
	;;
esac
