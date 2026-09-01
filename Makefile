# -----------------------------
# CONFIG
# -----------------------------
-include .env

APP_NAME      ?= iam-realm-provisioner
APP_ENV       ?= dev
GO            ?= go
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Private modules — resolved via SSH (git@github.com:) using the global URL
# rewrite. No tokens needed for local dev; an SSH key registered with the
# BCBP-SOLUTIONS-FZC-LLC org is required.
export GOPRIVATE  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*
export GONOSUMDB  ?= github.com/BCBP-SOLUTIONS-FZC-LLC/*

export APP_NAME APP_ENV BUILD_VERSION

TEST_UNIT_PKGS     := ./test/unit/...
TEST_POSTGRES_PKGS := ./test/postgres/...
TEST_INT_PKGS      := ./test/integration/...
TEST_E2E_PKGS      := ./test/e2e/...

# Every test/postgres/*.go test func calls t.Parallel() and spins its own
# testcontainers Postgres — capped, not GOMAXPROCS-wide, so a dev machine
# doesn't spin 10+ containers at once. Override on the CLI on a bigger box.
TEST_POSTGRES_PARALLEL ?= 4

# Every test/integration/*.go Keycloak test func calls t.Parallel() and spins
# its own testcontainers Keycloak (a JVM/Quarkus image, far heavier than
# Postgres). Left uncapped this defaults to GOMAXPROCS, which OOM-kills
# containers on dev/CI boxes with <16GB of Docker memory. Override on the CLI
# on a bigger box.
TEST_INTEGRATION_PARALLEL ?= 2

# White-box (package-internal) tests included in unit runs.
TEST_INTERNAL_PKGS := ./internal/adapter/inbound/http/... \
                      ./internal/adapter/inbound/consumer/... \
                      ./internal/adapter/outbound/eventbus/... \
                      ./internal/adapter/outbound/postgres/... \
                      ./internal/adapter/outbound/keycloak/... \
                      ./internal/adapter/outbound/openbao/... \
                      ./internal/adapter/outbound/orgmembership/... \
                      ./internal/adapter/outbound/s3/... \
                      ./internal/adapter/outbound/metrics/... \
                      ./internal/core/service/... \
                      ./internal/core/domain/... \
                      ./internal/core/port/... \
                      ./internal/eventschema/... \
                      ./pkg/...

COVER_PKG_LIST := $(shell $(GO) list ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup:
	@test -f .env || cp .env-example .env
	@mkdir -p .git/hooks
	@test -f .githooks/pre-commit && cp .githooks/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit || true
	@echo "Environment ready (.env)"

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup            - copy .env-example to .env if missing"
	@echo "  make tidy             - go mod tidy"
	@echo "  make fmt              - go fmt ./..."
	@echo "  make fmt-check        - verify gofmt formatting (mirrors CI)"
	@echo "  make vet              - go vet all packages"
	@echo "  make lint             - run golangci-lint (via go tool)"
	@echo "  make arch-lint        - run go-arch-lint (dependency-rule enforcement, §3.4)"
	@echo "  make test             - unit + postgres + integration tests (requires Docker)"
	@echo "  make test-ci          - test with race detector + coverage (used in CI)"
	@echo "  make test-unit        - unit tests only (no Docker required)"
	@echo "  make test-postgres    - Postgres + RLS integration tests (requires Docker)"
	@echo "  make test-integration - cross-layer integration tests (SNS/SQS/Keycloak via testcontainers)"
	@echo "  make test-e2e         - end-to-end tests (requires Docker)"
	@echo "  make race             - all tests with -race flag"
	@echo "  make run              - run the server locally (go run)"
	@echo "  make run-reconciler   - run one reconciler job locally, e.g. make run-reconciler JOB=trial-expiry"
	@echo "  make build            - compile both binaries to bin/"
	@echo "  make cover            - coverage HTML report"
	@echo "  make cover-func       - coverage summary by function"
	@echo "  make ci               - tidy + fmt-check + vet + lint + arch-lint + test-ci + build"
	@echo "  make docker-up        - start local infra (Postgres + LocalStack + Keycloak + OpenBao)"
	@echo "  make docker-down      - stop local containers"
	@echo "  make migrate-up       - apply pending domain migrations against DATABASE_MIGRATION_URL"
	@echo "  make migrate-down     - roll back one domain migration"
	@echo "  make migrate-create   - scaffold a new migration pair: make migrate-create NAME=add_x"
	@echo "  make mod-verify       - go mod verify"
	@echo "  make vuln-check       - govulncheck on internal + pkg"
	@echo "  make clean            - remove build artefacts"

# -----------------------------
# GO BASICS
# -----------------------------

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l cmd/ internal/ pkg/ 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "FAIL: unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "gofmt: all files formatted"

.PHONY: mod-verify
mod-verify:
	$(GO) mod verify

.PHONY: vuln-check
vuln-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./internal/... ./pkg/...

# -----------------------------
# LINT
# -----------------------------

.PHONY: lint
lint:
	@echo "Running linter..."
	$(GO) tool golangci-lint run

.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

# -----------------------------
# TESTS
# -----------------------------

.PHONY: _test-unit
_test-unit: | .coverage
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) \
	  -race -count=1 -timeout 120s \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/unit.out

.PHONY: _test-postgres
_test-postgres: | .coverage
	$(GO) test $(TEST_POSTGRES_PKGS) \
	  -tags=integration -race -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/postgres.out

.PHONY: _test-integration
_test-integration: | .coverage
	$(GO) test $(TEST_INT_PKGS) \
	  -tags=integration -race -count=1 -timeout 600s -parallel $(TEST_INTEGRATION_PARALLEL) \
	  -coverpkg=$(COVER_PKG_LIST) \
	  -coverprofile=.coverage/integration.out

.PHONY: test
test:
	$(MAKE) -j3 _test-unit-plain _test-postgres-plain _test-integration-plain

.PHONY: _test-unit-plain _test-postgres-plain _test-integration-plain
_test-unit-plain:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 120s -v
_test-postgres-plain:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v
_test-integration-plain:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 600s -parallel $(TEST_INTEGRATION_PARALLEL) -v

.PHONY: _merge-coverage
_merge-coverage:
	@python3 scripts/merge_coverage.py \
	  .coverage/unit.out .coverage/postgres.out .coverage/integration.out \
	  > coverage.out
	@echo "==> coverage.out merged from all suites (max-count strategy)"

.PHONY: test-ci
test-ci: | .coverage
	$(MAKE) -j3 _test-unit _test-postgres _test-integration
	$(MAKE) _merge-coverage

.PHONY: test-unit
test-unit:
	$(GO) test $(TEST_UNIT_PKGS) $(TEST_INTERNAL_PKGS) -count=1 -timeout 60s -v

.PHONY: test-postgres
test-postgres:
	$(GO) test $(TEST_POSTGRES_PKGS) -tags=integration -count=1 -timeout 300s -parallel $(TEST_POSTGRES_PARALLEL) -v

.PHONY: test-integration
test-integration:
	$(GO) test $(TEST_INT_PKGS) -tags=integration -count=1 -timeout 600s -parallel $(TEST_INTEGRATION_PARALLEL) -v

.PHONY: test-e2e
test-e2e:
	$(GO) test $(TEST_E2E_PKGS) -tags=e2e -count=1 -timeout 600s -v

.PHONY: race
race:
	$(MAKE) -j3 _test-unit _test-postgres _test-integration

# -----------------------------
# RUN
# -----------------------------

.PHONY: run
run:
	@-lsof -ti :$${APP_PORT:-8080} | xargs kill -9 2>/dev/null; true
	bash -c 'set -a && source .env && set +a && BUILD_VERSION=$(BUILD_VERSION) $(GO) run ./cmd/server'

.PHONY: run-reconciler
run-reconciler:
	@test -n "$(JOB)" || { echo "Usage: make run-reconciler JOB=<job-name>"; exit 1; }
	bash -c 'set -a && source .env && set +a && $(GO) run ./cmd/reconciler --job=$(JOB)'

# -----------------------------
# BUILD
# -----------------------------

.PHONY: build
build:
	@echo "Building binaries..."
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "-s -w -X main.buildVersion=$(BUILD_VERSION)" -o bin/$(APP_NAME)-server ./cmd/server
	$(GO) build -trimpath -ldflags "-s -w -X main.buildVersion=$(BUILD_VERSION)" -o bin/$(APP_NAME)-reconciler ./cmd/reconciler
	@echo "Verifying library packages compile..."
	$(GO) build ./internal/... ./pkg/...

# -----------------------------
# MIGRATIONS (standalone golang-migrate CLI — the server binary also
# self-migrates at startup via pgcommon/pkg/migrate; these targets are for
# local dev/CI convenience only, §4/§19)
# -----------------------------

MIGRATIONS_DIR := internal/adapter/outbound/postgres/migrations

.PHONY: migrate-up
migrate-up:
	@test -n "$(DATABASE_MIGRATION_URL)" || { echo "DATABASE_MIGRATION_URL not set"; exit 1; }
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
	  migrate/migrate -path=/migrations -database "$(DATABASE_MIGRATION_URL)" up

.PHONY: migrate-down
migrate-down:
	@test -n "$(DATABASE_MIGRATION_URL)" || { echo "DATABASE_MIGRATION_URL not set"; exit 1; }
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
	  migrate/migrate -path=/migrations -database "$(DATABASE_MIGRATION_URL)" down 1

.PHONY: migrate-create
migrate-create:
	@test -n "$(NAME)" || { echo "Usage: make migrate-create NAME=<description>"; exit 1; }
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations \
	  migrate/migrate create -ext sql -dir /migrations -seq $(NAME)

# -----------------------------
# DOCKER
# -----------------------------

.PHONY: docker-up
docker-up:
	@echo "Starting local PostgreSQL + LocalStack + Keycloak + OpenBao..."
	docker compose up -d postgres localstack keycloak openbao

.PHONY: docker-down
docker-down:
	@echo "Stopping local containers..."
	docker compose down

# -----------------------------
# CI
# -----------------------------

.PHONY: ci
ci: tidy fmt-check vet lint arch-lint test-ci build

# -----------------------------
# COVERAGE
# -----------------------------

.PHONY: cover
cover: test-ci
	$(GO) tool cover -html=coverage.out

.PHONY: cover-func
cover-func: test-ci
	$(GO) tool cover -func=coverage.out

# -----------------------------
# CLEAN
# -----------------------------

.coverage:
	@mkdir -p .coverage

.PHONY: clean
clean:
	rm -rf bin .coverage
	rm -f coverage.out coverage.html
