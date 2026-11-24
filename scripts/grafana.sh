#!/usr/bin/env bash
# Apply deploy/grafana: the LLM Gateway dashboard and its alert rules, into a
# folder of their own. Run it again after editing either file; it overwrites.
#
# GRAFANA_URL is the Grafana to write to (default: the local one from
# make obs-up). GRAFANA_TOKEN is a service account token with the Editor role
# and alert rule write access; without one, the local admin/admin is used.
# GRAFANA_PROM_UID names the Prometheus data source the alerts query, found
# by itself on Grafana Cloud (grafanacloud-prom) and locally (prometheus).
# All three come from the environment or ENV_FILE (.env.dev).

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require curl "It calls Grafana's HTTP API."

GRAFANA_URL="$(env_value GRAFANA_URL http://127.0.0.1:3000)"
GRAFANA_URL="${GRAFANA_URL%/}"
GRAFANA_TOKEN="$(env_value GRAFANA_TOKEN '')"
dir="$(dirname "${BASH_SOURCE[0]}")/../deploy/grafana"
folder=llm-gateway

auth=(-u admin:admin)
if [[ -n "$GRAFANA_TOKEN" ]]; then
	auth=(-H "Authorization: Bearer $GRAFANA_TOKEN")
fi

# api METHOD PATH [BODY_FILE]: prints the status code, and the body to stderr
# when the call failed.
api() {
	local out status
	out="$(mktemp)"
	status="$(curl -sS -o "$out" -w '%{http_code}' -X "$1" "${auth[@]}" \
		-H 'Content-Type: application/json' -H 'X-Disable-Provenance: true' \
		${3:+--data-binary "@$3"} "$GRAFANA_URL$2")"
	[[ "$status" == 2* ]] || cat "$out" >&2
	rm -f "$out"
	echo "$status"
}

prom_uid="$(env_value GRAFANA_PROM_UID '')"
if [[ -z "$prom_uid" ]]; then
	for uid in grafanacloud-prom prometheus; do
		if [[ "$(api GET "/api/datasources/uid/$uid" 2>/dev/null)" == 200 ]]; then
			prom_uid=$uid
			break
		fi
	done
fi
[[ -n "$prom_uid" ]] || die "no Prometheus data source found at $GRAFANA_URL. Set GRAFANA_PROM_UID in $ENV_FILE to its uid."

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '{"uid":"%s","title":"LLM Gateway"}' "$folder" >"$tmp/folder.json"
case "$(api POST /api/folders "$tmp/folder.json" 2>/dev/null)" in
2* | 409 | 412) ;;
*) [[ "$(api GET "/api/folders/$folder")" == 200 ]] || die "could not create the $folder folder" ;;
esac

{
	printf '{"folderUid":"%s","overwrite":true,"dashboard":' "$folder"
	cat "$dir/dashboard.json"
	printf '}'
} >"$tmp/dashboard.json"
status="$(api POST /api/dashboards/db "$tmp/dashboard.json")"
[[ "$status" == 200 ]] || die "the dashboard was refused ($status)"
log "dashboard applied: $GRAFANA_URL/d/llm-gateway"

sed "s/__PROM_UID__/$prom_uid/g" "$dir/alerts.json" >"$tmp/alerts.json"
status="$(api PUT "/api/v1/provisioning/folder/$folder/rule-groups/llm-gateway" "$tmp/alerts.json")"
[[ "$status" == 200 ]] || die "the alert rules were refused ($status)"
log "alert rules applied, querying $prom_uid. They notify through the default notification policy's contact point."
