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

# Pinned tool versions, read from the same file CI reads. Each tool is invoked
# through $(GOBIN) rather than by bare name so a differently-versioned copy
# earlier on PATH cannot silently take over.
GOLANGCI_LINT_VERSION := $(shell . ./versions.env && echo $$GOLANGCI_LINT_VERSION)
GOOSE_VERSION := $(shell . ./versions.env && echo $$GOOSE_VERSION)
SQLC_VERSION := $(shell . ./versions.env && echo $$SQLC_VERSION)
GOBIN := $(shell go env GOPATH)/bin

# Migrations run against the DATABASE_URL in .env.
DATABASE_URL := $(shell . ./.env 2>/dev/null && echo $$DATABASE_URL)
APP_DATABASE_URL := $(shell . ./.env 2>/dev/null && echo $$APP_DATABASE_URL)

.DEFAULT_GOAL := help

.PHONY: help check-prereqs setup dev up down restart health logs clean \
        fmt fmt-check lint lint-go typecheck test test-integration build ci tidy \
        migrate-up migrate-down migrate-status db-app-role sqlc sqlc-check

help: ## Show available commands
	@echo "Weave — available commands:"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo

# Creating .env from the committed template is a real file dependency, so any
# target that needs it can simply depend on `.env`.
#
# The symlink matters: Next.js reads .env only from its own directory, and its
# edge runtime and NEXT_PUBLIC_ inlining both need the file to be found that
# way — exporting the variables into the process, or loading them from
# next.config.ts, is too late for either. Linking keeps one file authoritative
# for both the Go API and the web application.
.env: .env.example
	@test -f .env || (cp .env.example .env && echo "Created .env from .env.example")
	@test -L apps/web/.env || ln -sf ../../.env apps/web/.env
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

# Installed from source so the linter is always built with the toolchain in
# versions.env; a prebuilt binary built with an older Go refuses to run against
# a module targeting a newer one.
lint-go: ## Run golangci-lint at the pinned version (CI gate)
	@test -x $(GOBIN)/golangci-lint && [ "$$($(GOBIN)/golangci-lint version --short 2>/dev/null)" = "$(GOLANGCI_LINT_VERSION)" ] \
		|| (echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)" \
		    && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION))
	@$(GOBIN)/golangci-lint run $(GO_PKGS)

typecheck: ## Type-check the web workspace (CI gate)
	@npm run typecheck

test: ## Run unit tests (CI gate)
	@go test -race $(GO_PKGS)

# Integration tests need a migrated database. They skip themselves when
# TEST_DATABASE_URL is unset, which is what keeps `make test` runnable without
# Docker.
# Two URLs: most tests connect as the owner to exercise the application's own
# filtering, while the row-level-security tests connect as the application role
# so the policies actually apply to them.
# Both URLs are required rather than optional. An .env created before
# APP_DATABASE_URL existed is not updated by the .env rule above, so the
# variable would be empty, every RLS test would skip itself, and the command
# would report success having proved nothing about the policies.
test-integration: .env ## Run integration tests against the local database
	@test -n "$(DATABASE_URL)" || (echo "DATABASE_URL is empty. Check .env against .env.example." && exit 1)
	@test -n "$(APP_DATABASE_URL)" || (echo "APP_DATABASE_URL is empty, so the row-level-security tests would skip. Add it to .env — see .env.example." && exit 1)
	@TEST_DATABASE_URL="$(DATABASE_URL)" TEST_APP_DATABASE_URL="$(APP_DATABASE_URL)" \
		go test -race -count=1 -run 'Integration|Test' $(GO_PKGS)

build: ## Build the API binary and the web application (CI gate)
	@go build $(GO_PKGS)
	@npm run build

tidy: ## Ensure go.mod and go.sum are current
	@go mod tidy

# --- Database ----------------------------------------------------------------

# Installs the pinned tool only when the pinned version is not already present.
define ensure_tool
	@command -v $(GOBIN)/$(1) >/dev/null 2>&1 && [ "$$($(GOBIN)/$(1) $(3) 2>/dev/null | grep -o '$(2)')" = "$(2)" ] \
		|| (echo "Installing $(1) $(2)" && go install $(4)@v$(2))
endef

# The migration runner is a program in this repository (services/migrate), not
# the goose CLI: the CLI links every database driver goose supports, which is a
# large build and dependency surface for a PostgreSQL-only project.
migrate-up: .env ## Apply all pending database migrations
	@DATABASE_URL="$(DATABASE_URL)" go run ./services/migrate up
	@$(MAKE) --no-print-directory db-app-role

migrate-down: .env ## Roll back the most recent migration
	@DATABASE_URL="$(DATABASE_URL)" go run ./services/migrate down

db-app-role: .env ## Set the application role's password from APP_DATABASE_URL
	@DATABASE_URL="$(DATABASE_URL)" APP_DATABASE_URL="$(APP_DATABASE_URL)" go run ./services/migrate app-role

migrate-status: .env ## Show which migrations have been applied
	@DATABASE_URL="$(DATABASE_URL)" go run ./services/migrate status

sqlc: ## Regenerate the database access layer from db/queries
	$(call ensure_tool,sqlc,$(SQLC_VERSION),version,github.com/sqlc-dev/sqlc/cmd/sqlc)
	@$(GOBIN)/sqlc generate
	@echo "Generated internal/adapters/postgres/postgresdb."

sqlc-check: sqlc ## Fail when committed generated code is stale (CI gate)
	@git diff --exit-code -- internal/adapters/postgres/postgresdb \
		|| (echo "Generated code is out of date. Commit the result of 'make sqlc'." && exit 1)

ci: fmt-check lint lint-go sqlc-check typecheck test build ## Run every local quality gate
	@echo "All quality gates passed."
