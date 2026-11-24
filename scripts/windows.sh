#!/usr/bin/env bash
# Rebuild the context window catalogue from OpenRouter's and LiteLLM's
# published model data. Commit the result.
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./cmd/windows "$@"
