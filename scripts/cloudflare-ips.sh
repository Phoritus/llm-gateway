#!/usr/bin/env bash
# Writes Cloudflare's edge ranges to deploy/trusted-proxies.env, which both
# Caddy and the gateway read as TRUSTED_PROXIES.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require curl "It is used to download Cloudflare's IP lists."

fetch() {
	curl -fsS --max-time 20 "$1" | tr -d '\r' | grep -E '^[0-9a-fA-F:.]+/[0-9]+$' ||
		die "could not read $1"
}

ranges="$( {
	fetch https://www.cloudflare.com/ips-v4
	fetch https://www.cloudflare.com/ips-v6
} )"
count="$(wc -l <<<"$ranges" | tr -d ' ')"
((count >= 10)) || die "only $count ranges came back; refusing to write a short list"

out=deploy/trusted-proxies.env
printf 'TRUSTED_PROXIES=%s\n' "$(paste -sd' ' - <<<"$ranges")" >"$out"
log "wrote $count ranges to $out"
