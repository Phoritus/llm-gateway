#!/usr/bin/env bash
# Usage: scripts/vm.sh ssh | deploy | logs
#
#   ssh     open a shell on the VM, in ~/llm-gateway/deploy (the default)
#   deploy  pull main on the VM, then rebuild and restart the stack in deploy/
#   logs    follow the gateway's logs
#
# VM_HOST, VM_USER and VM_KEY come from the environment or ENV_FILE
# (.env.dev).

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require ssh "It ships with Git for Windows and with every Linux and macOS install."

VM_HOST="$(env_value VM_HOST '')"
VM_USER="$(env_value VM_USER azureuser)"
VM_KEY="$(env_value VM_KEY llm-gateway_key.pem)"
[[ -n "$VM_HOST" ]] || die "VM_HOST is not set. Add VM_HOST=<the VM's DNS name or public IP> to $ENV_FILE."

# ~ in a value read from a file is not expanded by the shell.
VM_KEY="${VM_KEY/#\~/$HOME}"
[[ -f "$VM_KEY" ]] || die "VM_KEY=$VM_KEY does not exist. Set VM_KEY in $ENV_FILE to the .pem file Azure gave you."
target="$VM_USER@$VM_HOST"

case "${1:-ssh}" in
ssh)
	exec ssh -t -i "$VM_KEY" "$target" 'cd ~/llm-gateway/deploy 2>/dev/null; exec bash -l'
	;;
deploy)
	log "deploying main to $VM_HOST"
	ssh -i "$VM_KEY" "$target" 'set -e
		cd ~/llm-gateway
		git pull --ff-only
		cd deploy
		docker compose up -d --build
		docker compose ps'
	;;
logs)
	exec ssh -t -i "$VM_KEY" "$target" 'cd ~/llm-gateway/deploy && docker compose logs -f --tail=100 gateway'
	;;
*)
	die "usage: scripts/vm.sh ssh | deploy | logs"
	;;
esac
