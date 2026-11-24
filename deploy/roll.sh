#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

compose() { docker compose "$@"; }

healthy() {
	local svc=$1
	for _ in $(seq 90); do
		if compose exec -T caddy wget -q -O /dev/null "http://$svc:8080/healthz" 2>/dev/null; then
			sleep 2
			return 0
		fi
		sleep 1
	done
	echo "$svc did not become healthy in 90s; the other gateway is still serving" >&2
	compose logs --tail=50 "$svc" >&2
	return 1
}

compose build gateway-a
compose up -d --no-deps redis
if ! compose ps --status running --services | grep -qx caddy; then
	compose up -d --no-build
	healthy gateway-a
	healthy gateway-b
	exit 0
fi

for svc in gateway-a gateway-b; do
	compose up -d --no-deps --no-build "$svc"
	healthy "$svc"
done
compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
project=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$(compose ps -q caddy)")
old=$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=gateway")
if [[ -n "$old" ]]; then
	sleep 2
	docker stop -t 35 $old
fi
compose up -d --no-deps --no-build --remove-orphans gateway-a gateway-b redis
compose ps
