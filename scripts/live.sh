#!/usr/bin/env bash
# Check the real provider adapters through a running development gateway,
# using the provider keys in the env file (GROQ_API_KEY, GEMINI_API_KEY, ...).
#
# Usage: scripts/live.sh            (start the gateway first: make run)
#
# Spends a handful of free-tier requests per vendor with a key, and creates a
# throwaway account in the development database each run.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require uv "Install it from https://docs.astral.sh/uv/."
load_env || die "could not read $ENV_FILE"
[[ "${APP_ENV:-development}" == "development" ]] ||
	die "APP_ENV=$APP_ENV: the live check confirms its account directly in the development database"

uv run --quiet --script scripts/live/live.py
