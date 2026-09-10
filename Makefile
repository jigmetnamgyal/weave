# Weave task runner.
#
# `make dev` is the single documented command that starts everything.
# Run `make help` for the full list.

SHELL := /bin/bash

COMPOSE_FILE := infra/docker-compose.yml
COMPOSE := docker compose --file $(COMPOSE_FILE) --env-file .env
WEB_WORKSPACE := @weave/web

# Go package and directory selectors are explicit rather than `./...` because
# npm workspaces hoist node_modules to the repository root, and one transitive
# npm package vendors a .go file that would otherwise be treated as ours.
GO_PKGS := ./services/... ./internal/...
GO_DIRS := services internal

.DEFAULT_GOAL := help

.PHONY: help check-prereqs setup dev up down restart health logs clean \
        fmt fmt-check lint typecheck test build ci tidy

help: ## Show available commands
	@echo "Weave — available commands:"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo

# Creating .env from the committed template is a real file dependency, so any
# target that needs it can simply depend on `.env`.
.env: .env.example
	@test -f .env || (cp .env.example .env && echo "Created .env from .env.example")
	@touch .env

check-prereqs: ## Verify the local toolchain matches versions.env
	@./scripts/check-prereqs.sh

setup: check-prereqs .env ## Install dependencies and prepare the workspace
	@npm install
	@go mod download
	@echo "Setup complete. Run 'make dev' to start the stack."

dev: ## Start dependencies, the Go API and the web shell (one command)
	@./scripts/dev.sh

up: .env ## Start the dependency containers and wait until healthy
	@$(COMPOSE) up --detach --wait
	@echo "Dependencies are healthy."

down: .env ## Stop the dependency containers
	@$(COMPOSE) down

restart: down up ## Recreate the dependency containers

health: ## Report health of every dependency and the API probes
	@./scripts/health.sh

logs: .env ## Follow dependency container logs
	@$(COMPOSE) logs --follow

clean: .env ## Stop containers and delete their volumes (destroys local data)
	@$(COMPOSE) down --volumes --remove-orphans
	@rm -rf apps/web/.next
	@echo "Local containers, volumes and build output removed."

# --- Quality gates -----------------------------------------------------------
# `make ci` runs the same gates as .github/workflows/ci.yml.

fmt: ## Format Go and web sources in place
	@gofmt -w $(GO_DIRS)
	@npm run format

fmt-check: ## Check formatting without writing (CI gate)
	@echo "==> gofmt"
	@test -z "$$(gofmt -l $(GO_DIRS) | tee /dev/stderr)" || (echo "Run 'make fmt'." && exit 1)
	@echo "==> prettier"
	@npm run format:check

lint: ## Lint Go and web sources (CI gate)
	@echo "==> go vet"
	@go vet $(GO_PKGS)
	@echo "==> eslint"
	@npm run lint

typecheck: ## Type-check the web workspace (CI gate)
	@npm run typecheck

test: ## Run unit tests (CI gate)
	@go test -race $(GO_PKGS)

build: ## Build the API binary and the web application (CI gate)
	@go build $(GO_PKGS)
	@npm run build

tidy: ## Ensure go.mod and go.sum are current
	@go mod tidy

ci: fmt-check lint typecheck test build ## Run every local quality gate
	@echo "All quality gates passed."
