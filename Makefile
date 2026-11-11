# Short commands for everyday work on this repo. Run "make" to list them.
#
# Each target delegates to a script in scripts/, so every command is also
# available without GNU make — "scripts/run.sh", "scripts/deps.sh up" and so
# on do the same work.
#
# make itself needs bash on PATH. On Windows, run it from Git Bash; if make is
# missing, install it with:  winget install ezwinports.make

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO ?= go
BIN := bin/gateway

.PHONY: help
help: ## List the available commands
	@printf 'Usage: make <target>\n\n'
	@grep -hE '^[a-zA-Z0-9_.-]+:.*## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*## "} {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@printf '\nFirst time here:  make setup && make run\n'

# --- Getting started ---------------------------------------------------------

.PHONY: setup
setup: env deps-up ## Create .env and start Postgres and Redis
	@printf '\nReady. Run "make run" to start the service.\n'

.PHONY: env
env: ## Write .env with freshly generated secrets (keeps an existing one)
	@scripts/env-init.sh

.PHONY: env-secrets
env-secrets: ## Fill in only a missing or placeholder MASTER_KEY/SESSION_SECRET
	@scripts/env-init.sh --fill-secrets

.PHONY: run
run: ## Run the service with .env loaded
	@scripts/run.sh

.PHONY: dev
dev: deps-up run ## Start the dependencies, then the service

.PHONY: build
build: ## Compile the service to bin/gateway
	$(GO) build -o $(BIN) ./cmd/gateway

# --- Dependencies ------------------------------------------------------------

.PHONY: deps-up
deps-up: ## Start Postgres and Redis and wait until both are healthy
	@scripts/deps.sh up

.PHONY: deps-down
deps-down: ## Stop Postgres and Redis, keeping their data
	@scripts/deps.sh down

.PHONY: deps-reset
deps-reset: ## Stop them and delete their volumes (drops all local data)
	@scripts/deps.sh reset

.PHONY: deps-status
deps-status: ## Show the dependency containers
	@scripts/deps.sh status

.PHONY: psql
psql: ## Open a psql shell on the development database
	@scripts/deps.sh psql

.PHONY: redis-cli
redis-cli: ## Open a redis-cli shell
	@scripts/deps.sh redis-cli

# --- Observability -----------------------------------------------------------

.PHONY: obs-up
obs-up: ## Start Grafana, Prometheus, Tempo and Loki and wait for Grafana
	@scripts/obs.sh up

.PHONY: obs-down
obs-down: ## Stop the observability stack
	@scripts/obs.sh down

.PHONY: obs-status
obs-status: ## Show the observability container
	@scripts/obs.sh status

# --- Checks ------------------------------------------------------------------

.PHONY: check
check: fmt-check vet build test ## Everything that must pass before a commit

.PHONY: fmt
fmt: ## Format the tree
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if anything is not gofmt-clean
	$(if $(strip $(shell gofmt -l .)),$(error not gofmt-clean: $(shell gofmt -l .)),echo gofmt-clean)

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: test
test: ## Run the unit tests (integration tests skip)
	@scripts/test.sh unit

.PHONY: test-int
test-int: ## Run every test, starting a throwaway Postgres and Redis
	@scripts/test.sh int

.PHONY: test-race
test-race: ## Run every test under the race detector (needs cgo)
	@scripts/test.sh race

.PHONY: test-db-up
test-db-up: ## Start the throwaway test Postgres on its own port
	@scripts/test.sh db-up

.PHONY: test-db-down
test-db-down: ## Remove the throwaway test Postgres
	@scripts/test.sh db-down

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

# --- Packaging ---------------------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the container image
	docker build -t llm-gateway .

.PHONY: clean
clean: ## Remove build output
	rm -rf bin
