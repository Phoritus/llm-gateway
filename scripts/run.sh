#!/usr/bin/env bash
# Run the gateway with .env loaded.
#
# The service reads configuration from the environment only, so something has
# to put .env into it. This does, and creates .env on first run so that a
# fresh checkout is one command away from a running service.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require go "Install Go 1.26 or newer."

if [[ ! -f .env ]]; then
	log "no .env found; generating one"
	"$REPO_ROOT/scripts/env-init.sh"
fi

load_env || die "could not read .env"

log "starting the gateway on port ${PORT:-8080} (APP_ENV=${APP_ENV:-development})"
exec go run ./cmd/gateway "$@"
