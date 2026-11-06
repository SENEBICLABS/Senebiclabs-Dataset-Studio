# Dataset Studio: local development and verification. No deploy targets.
#
#   make help        list targets
#   make up          start local PostgreSQL (compose) and wait until healthy
#   make check       full M0 gate: toolchain, tidy, fmt, vet, lint, vuln,
#                    unit tests, build, integration + e2e tests

SHELL       := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

# Go toolchain. Override when `go` on PATH is unusable, e.g.
#   make check GO=/snap/go/current/bin/go
GO ?= go

MODULE      := github.com/SENEBICLABS/Senebiclabs-Dataset-Studio
BIN_DIR     := $(CURDIR)/bin
TOOLS_BIN   := $(CURDIR)/.tools/bin

# Pinned tool versions (see the dependency table in the Phase 1 plan).
# Each version installs into its own directory, so bumping a version installs
# the new binary instead of silently reusing a stale one.
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.9.0
GOLANGCI_LINT := $(TOOLS_BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)/golangci-lint
GOVULNCHECK   := $(TOOLS_BIN)/govulncheck-$(GOVULNCHECK_VERSION)/govulncheck
# sqlc is pinned in tools/go.mod (plan 3.1); this version must match it.
SQLC_VERSION  := v1.31.1
SQLC          := $(TOOLS_BIN)/sqlc-$(SQLC_VERSION)/sqlc

# Build identity injected into every binary.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT)

# Local settings. .env is plain KEY=value; created from .env.example by `make env`.
# Values read from .env become make variables but are NOT exported globally:
#   - docker compose reads .env itself (--env-file);
#   - STUDIO_TEST_DATABASE_URL (bootstrap superuser credentials) and the
#     STUDIO_DB_*_PASSWORD role passwords are passed only to the test command
#     in test-integration and to db-setup;
#   - application settings (APP_ENV_VARS) are passed only to run-api/run-worker.
-include .env
STUDIO_PG_PORT ?= 55432
STUDIO_TEST_DATABASE_URL ?= postgres://studio_bootstrap:$(STUDIO_PG_BOOTSTRAP_PASSWORD)@127.0.0.1:$(STUDIO_PG_PORT)/studio?sslmode=disable

# Login-role passwords (plan 6.1). Local placeholders; override them in .env.
# They are passed only to `db-setup` and the integration test command.
STUDIO_DB_MIGRATOR_PASSWORD ?= local-dev-only-migrator
STUDIO_DB_APP_PASSWORD      ?= local-dev-only-app-role
STUDIO_DB_WORKER_PASSWORD   ?= local-dev-only-worker
STUDIO_DB_BACKUP_PASSWORD   ?= local-dev-only-backup
STUDIO_DB_READONLY_PASSWORD ?= local-dev-only-readonly
STUDIO_DB_NAME              ?= studio
DB_PASSWORD_ENV = STUDIO_DB_MIGRATOR_PASSWORD='$(STUDIO_DB_MIGRATOR_PASSWORD)' \
	STUDIO_DB_APP_PASSWORD='$(STUDIO_DB_APP_PASSWORD)' STUDIO_DB_WORKER_PASSWORD='$(STUDIO_DB_WORKER_PASSWORD)' \
	STUDIO_DB_BACKUP_PASSWORD='$(STUDIO_DB_BACKUP_PASSWORD)' STUDIO_DB_READONLY_PASSWORD='$(STUDIO_DB_READONLY_PASSWORD)'
PG_LOCAL_URL = postgres://$(1):$(2)@127.0.0.1:$(STUDIO_PG_PORT)/$(3)?sslmode=disable

# Settings the binaries read. Only variables that are defined (in .env or the
# calling environment) are passed, so unset ones keep the binaries' defaults.
APP_ENV_VARS := STUDIO_LOG_LEVEL STUDIO_LOG_FORMAT STUDIO_HTTP_ADDR STUDIO_DATABASE_URL \
	STUDIO_SHUTDOWN_TIMEOUT STUDIO_READINESS_TIMEOUT STUDIO_HTTP_READ_HEADER_TIMEOUT \
	STUDIO_HTTP_READ_TIMEOUT STUDIO_HTTP_WRITE_TIMEOUT STUDIO_HTTP_IDLE_TIMEOUT \
	STUDIO_SESSION_IDLE_TIMEOUT STUDIO_AUDIT_HMAC_KEY
APP_ENV = $(foreach v,$(APP_ENV_VARS),$(if $(filter undefined,$(origin $(v))),,$(v)='$($(v))'))

COMPOSE := docker compose --env-file .env -f deploy/compose.yaml

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-18s %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Environment

.PHONY: toolchain-check
toolchain-check: ## Fail unless the Go toolchain actually runs (guards against a silent `go`)
	@v="$$($(GO) env GOVERSION 2>/dev/null || true)"; \
	if [[ -z "$$v" ]]; then \
	  echo "error: '$(GO) env GOVERSION' printed nothing; the Go toolchain is not usable." >&2; \
	  echo "       Retry with: make <target> GO=/snap/go/current/bin/go" >&2; exit 1; \
	fi; \
	echo "go toolchain: $$v ($(GO))"

env: .env ## Create .env from .env.example if missing
.env:
	cp .env.example .env
	@echo "created .env from .env.example (local-only values)"

.PHONY: tools
tools: $(GOLANGCI_LINT) $(GOVULNCHECK) $(SQLC) ## Install pinned lint, vuln and sqlc tools into .tools/bin

$(GOLANGCI_LINT):
	GOBIN=$(dir $@) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOVULNCHECK):
	GOBIN=$(dir $@) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

# sqlc compiles a C parser (cgo) and needs roughly 2 GB of free disk to build.
$(SQLC): tools/go.mod
	@grep -q 'github.com/sqlc-dev/sqlc $(SQLC_VERSION)' tools/go.mod || { echo "error: tools/go.mod does not pin sqlc $(SQLC_VERSION)" >&2; exit 1; }
	cd tools && $(GO) build -o $@ github.com/sqlc-dev/sqlc/cmd/sqlc

# ---------------------------------------------------------------------------
# Local PostgreSQL

.PHONY: up
up: .env ## Start local PostgreSQL and wait until healthy
	$(COMPOSE) up -d --wait

.PHONY: down
down: ## Stop local services (data volume is kept)
	$(COMPOSE) down

.PHONY: ps
ps: ## Show local service status
	$(COMPOSE) ps

.PHONY: db-setup
db-setup: build ## Bootstrap roles + database as superuser, then migrate as studio_migrator
	@$(COMPOSE) ps --status running --services 2>/dev/null | grep -qx postgres || { echo "error: local PostgreSQL is not running; run 'make up' first" >&2; exit 1; }
	@echo "studioctl db bootstrap  (STUDIO_BOOTSTRAP_DATABASE_URL=<redacted> STUDIO_DB_*_PASSWORD=<redacted>)"
	@STUDIO_BOOTSTRAP_DATABASE_URL='$(call PG_LOCAL_URL,studio_bootstrap,$(STUDIO_PG_BOOTSTRAP_PASSWORD),postgres)' \
	  STUDIO_DB_NAME='$(STUDIO_DB_NAME)' $(DB_PASSWORD_ENV) $(BIN_DIR)/studioctl db bootstrap
	@echo "studioctl migrate  (STUDIO_MIGRATOR_DATABASE_URL=<redacted>)"
	@STUDIO_MIGRATOR_DATABASE_URL='$(call PG_LOCAL_URL,studio_migrator,$(STUDIO_DB_MIGRATOR_PASSWORD),$(STUDIO_DB_NAME))' \
	  $(BIN_DIR)/studioctl migrate

.PHONY: db-destroy
db-destroy: ## Stop services AND delete the local data volume (requires CONFIRM=yes)
	@[[ "$(CONFIRM)" == "yes" ]] || { echo "refusing: this deletes the local database volume. Re-run with CONFIRM=yes" >&2; exit 1; }
	$(COMPOSE) down -v

# ---------------------------------------------------------------------------
# Quality gates

.PHONY: tidy-check
tidy-check: toolchain-check ## Fail if go.mod/go.sum (root and tools/) are not tidy
	$(GO) mod tidy -diff
	cd tools && $(GO) mod tidy -diff

.PHONY: sqlc
sqlc: $(SQLC) ## Regenerate sqlc stores from db/queries and migrations
	$(SQLC) generate

.PHONY: sqlc-check
sqlc-check: $(SQLC) ## Fail if committed sqlc output is stale
	$(SQLC) diff

.PHONY: fmt-check
fmt-check: toolchain-check ## Fail if any file is not gofmt-formatted
	@gofmt_bin="$$($(GO) env GOROOT)/bin/gofmt"; \
	out="$$("$$gofmt_bin" -l $$(git ls-files --cached --others --exclude-standard '*.go'))"; \
	if [[ -n "$$out" ]]; then echo "not gofmt-formatted:" >&2; echo "$$out" >&2; exit 1; fi

.PHONY: vet
vet: toolchain-check ## go vet (including integration-tagged files)
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## golangci-lint (pinned)
	$(GOLANGCI_LINT) run ./...

.PHONY: vuln
vuln: $(GOVULNCHECK) ## govulncheck (pinned)
	$(GOVULNCHECK) ./...

.PHONY: test
test: toolchain-check ## Unit tests with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: build
build: toolchain-check ## Build all binaries into ./bin
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/ ./cmd/...

.PHONY: test-integration
test-integration: build ## Integration + e2e tests (needs `make up`)
	@$(COMPOSE) ps --status running --services 2>/dev/null | grep -qx postgres || { echo "error: local PostgreSQL is not running; run 'make up' first" >&2; exit 1; }
	@echo "STUDIO_BIN_DIR=$(BIN_DIR) STUDIO_EXPECT_COMMIT=$(COMMIT) STUDIO_PG_CONTAINER=<compose postgres> STUDIO_TEST_DATABASE_URL=<redacted> STUDIO_DB_*_PASSWORD=<redacted> $(GO) test -race -count=1 -tags integration ./..."
	@STUDIO_BIN_DIR='$(BIN_DIR)' STUDIO_EXPECT_COMMIT='$(COMMIT)' STUDIO_TEST_DATABASE_URL='$(STUDIO_TEST_DATABASE_URL)' \
	  STUDIO_PG_CONTAINER="$$($(COMPOSE) ps -q postgres)" \
	  $(DB_PASSWORD_ENV) $(GO) test -race -count=1 -tags integration ./...

.PHONY: check
check: toolchain-check tidy-check sqlc-check fmt-check vet lint vuln test build test-integration ## Full gate
	@echo "make check: all gates passed"

# ---------------------------------------------------------------------------
# Run locally

.PHONY: run-api
run-api: build ## Run studio-api with the application settings from .env
	@echo "$(BIN_DIR)/studio-api  (env: $(APP_ENV_VARS) where set)"
	@env $(APP_ENV) $(BIN_DIR)/studio-api

.PHONY: run-worker
run-worker: build ## Run studio-worker with the application settings from .env
	@echo "$(BIN_DIR)/studio-worker  (env: $(APP_ENV_VARS) where set)"
	@env $(APP_ENV) $(BIN_DIR)/studio-worker

.PHONY: clean
clean: ## Remove build outputs (keeps .tools and the database)
	rm -rf $(BIN_DIR)
