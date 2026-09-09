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

# Black-box service/domain suite (package unit_test) — see CONTRIBUTING
# "Test layout" for what belongs here vs. TEST_INTERNAL_PKGS below.
TEST_UNIT_PKGS     := ./test/unit/...
TEST_POSTGRES_PKGS := ./test/postgres/...
TEST_INT_PKGS      := ./test/integration/...
TEST_E2E_PKGS      := ./test/e2e/...
TEST_LOAD_PKGS     := ./test/load/...

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

# White-box (package-internal) tests included in unit runs, alongside the
# black-box suite in TEST_UNIT_PKGS. ./cmd/... is listed so composition-root
# helpers (the gauge exporters, reconciler jobs) are testable in this target
# and counted in COVER_PKG_LIST below, not just under a bare `go test ./...`.
TEST_INTERNAL_PKGS := ./cmd/... \
                      ./internal/adapter/inbound/http/... \
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

COVER_PKG_LIST := $(shell $(GO) list ./cmd/... ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

# -----------------------------
# SETUP
# -----------------------------

.PHONY: setup
setup: install-hooks
	@test -f .env || cp .env-example .env
	@echo "Environment ready (.env)"

.PHONY: install-hooks
install-hooks:
	@mkdir -p .git/hooks
	@cp .githooks/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Installed git hooks"

.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make setup            - copy .env-example to .env if missing; install .githooks/pre-commit"
	@echo "  make install-hooks    - install local git hooks (.githooks -> .git/hooks); also run by make setup"
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
	@echo "  make test-load        - RP-3 migration load test (fake Keycloak, no Docker); LOAD_TEST_FULL=1 for N=500"
	@echo "  make race             - all tests with -race flag"
	@echo "  make run              - run the server locally (go run)"
	@echo "  make run-reconciler   - run one reconciler job locally, e.g. make run-reconciler JOB=trial-expiry"
	@echo "  make mock             - start BOTH mocks (Keycloak :8091 + O&M :8082) in one command"
	@echo "  make mock OM_MODE=lapses - start both mocks with O&M in lapses mode"
	@echo "  make mock-keycloak    - start Keycloak mock only (:8091)"
	@echo "  make mock-org         - start O&M mock only (:8082)"
	@echo "  make build            - compile both binaries to bin/"
	@echo "  make cover            - coverage HTML report"
	@echo "  make cover-func       - coverage summary by function"
	@echo "  make ci               - tidy + fmt-check + vet + lint + arch-lint + test-ci + build"
	@echo "  make docker-up        - start Postgres + LocalStack + Keycloak + OpenBao (community)"
	@echo "  make docker-up-pro    - start Postgres + LocalStack Pro only (use with 'make mock')"
	@echo "  make docker-down      - stop local containers"
	@echo "  make migrate-up       - apply pending domain migrations against DATABASE_MIGRATION_URL"
	@echo "  make migrate-down     - roll back one domain migration"
	@echo "  make migrate-create   - scaffold a new migration pair: make migrate-create NAME=add_x"
	@echo "  make mod-verify       - go mod verify"
	@echo "  make vuln-check       - govulncheck on internal + pkg"
	@echo "  make clean            - remove build artefacts"
	@echo ""
	@echo "Schema governance (platform-schemagov 0.4, shared iam-tenant-events registry):"
	@echo "  make schema-pull      - pull the platform-schemagov Docker image"
	@echo "  make extract-schemas  - derive internal/eventschema/*.json from api/asyncapi.yaml"
	@echo "  make schema-validate  - validate AsyncAPI + event schemas — 8 passes (no AWS required)"
	@echo "  make schema-diff      - diff two schema files: CURRENT=<path> PROPOSED=<path>"
	@echo "  make schema-register  - register this service's 9 event schemas to Glue (requires AWS/LocalStack)"
	@echo "  make schema-verify    - pre-deploy check: fail if any of RP's 9 schemas is missing (requires AWS)"
	@echo "  make schema-prune     - dry-run: list orphaned Glue schemas (requires AWS; excludes O&M's TenantCreated/TrialStarted)"
	@echo ""
	@echo "Swagger:"
	@echo "  make swag             - generate Swagger docs from annotations (output: docs/swagger/)"
	@echo "  make swag-check       - fail if Swagger regeneration changes docs/swagger/"

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

# RP-3 migration load test (LLD §14.6) — a fast default (N=80/N=320)
# validating the Keycloak Admin API rate limiter + linear scaling; not part
# of `make test`/`make test-ci` (expensive, deliberately separate, like
# test-e2e). Set LOAD_TEST_FULL=1 to also run the LLD's literal N=500 case.
.PHONY: test-load
test-load:
	$(GO) test $(TEST_LOAD_PKGS) -tags=load -count=1 -timeout 600s -v

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

.PHONY: mock
mock:
	@echo "Starting Keycloak mock (:8091) + Org & Membership mock (:8082)..."
	@-lsof -ti :8091 | xargs kill -9 2>/dev/null; true
	@-lsof -ti :8082 | xargs kill -9 2>/dev/null; true
	cd ../mock-servers && python3 run.py $(if $(OM_MODE),--om-mode $(OM_MODE),)

.PHONY: mock-keycloak
mock-keycloak:
	@echo "Starting Keycloak Admin mock on port 8091..."
	@-lsof -ti :8091 | xargs kill -9 2>/dev/null; true
	python3 ../mock-servers/mock_keycloak.py --port 8091

.PHONY: mock-org
mock-org:
	@echo "Starting Org & Membership mock on port 8082..."
	@-lsof -ti :8082 | xargs kill -9 2>/dev/null; true
	python3 ../mock-servers/mock_org.py --port 8082 $(if $(OM_MODE),--mode $(OM_MODE),)

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
	@echo "Starting local PostgreSQL + LocalStack + Keycloak + OpenBao (community)..."
	docker compose up -d postgres localstack keycloak openbao

.PHONY: docker-up-pro
docker-up-pro:
	@echo "Starting Postgres + LocalStack Pro (Glue + KMS) + Keycloak + OpenBao..."
	@grep -q '^LOCALSTACK_AUTH_TOKEN=.\+' .env 2>/dev/null || { echo "ERROR: LOCALSTACK_AUTH_TOKEN not set in .env"; exit 1; }
	docker compose -f docker-compose.pro.yml up -d postgres localstack keycloak openbao

.PHONY: docker-down
docker-down:
	@echo "Stopping local containers..."
	docker compose down

# -----------------------------
# SCHEMA GOVERNANCE
# -----------------------------
# api/asyncapi.yaml is the single source of truth for this service's 9
# produced event schemas. The `iam-tenant-events` Glue registry is SHARED
# with iam-org-membership (O&M), which registers two more schema names there
# (TenantCreated, TrialStarted) that this service does not own and must
# never register, diff, verify, or prune (§7.3.1, RP-D5).
SCHEMA_GOV_IMAGE ?= ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4

# schema-pull: pull the platform-schemagov Docker image.
.PHONY: schema-pull
schema-pull:
	docker pull "$(SCHEMA_GOV_IMAGE)"

# extract-schemas: derive internal/eventschema/*.json from api/asyncapi.yaml.
# api/asyncapi.yaml is the single source of truth for event payload schemas.
# Re-run whenever asyncapi.yaml changes; commit the updated JSON files alongside.
# The JSON files are read by schema-gov validate --schema-dir internal/eventschema.
.PHONY: extract-schemas
extract-schemas:
	@echo "Extracting event schemas from api/asyncapi.yaml..."
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" extract \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/eventschema
	@echo "Done. Run 'git add internal/eventschema/' to stage the changes."

# schema-validate: validate AsyncAPI spec + event schemas — 8 passes:
# (1) structure, (2) draft-07, (3) enum drift, (4) lifecycle annotations,
# (5) open-schema guard, (6) consumer strict-mode, (7) coverage, (8) AsyncAPI structure.
# No AWS credentials needed. Pass 4 only warns (does not fail) on schemas with
# no x-lifecycle annotation — this service's 9 events are frozen (LLD §25)
# and deliberately carry none yet (see ARCHITECTURE.md).
.PHONY: schema-validate
schema-validate: extract-schemas
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" validate \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/eventschema

# schema-diff: show compatibility diff between two schema files.
# Usage: make schema-diff CURRENT=<path-to-current.json> PROPOSED=<path-to-proposed.json>
#        Optionally: SCHEMA_NAME=<name> (defaults to the PROPOSED filename stem)
.PHONY: schema-diff
schema-diff:
	@test -n "$(CURRENT)" && test -n "$(PROPOSED)" || { \
	  echo "Usage: make schema-diff CURRENT=<current.json> PROPOSED=<proposed.json> [SCHEMA_NAME=<name>]"; \
	  exit 1; \
	}
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" diff \
	  --current     "$(CURRENT)" \
	  --proposed    "$(PROPOSED)" \
	  --schema-name "$(or $(SCHEMA_NAME),$(notdir $(basename $(PROPOSED))))"

# schema-register: register this service's 9 event schemas to Glue (requires
# AWS credentials or LocalStack). Only ever registers the files present under
# internal/eventschema/ — never touches O&M's TenantCreated/TrialStarted,
# which have no file here and are therefore never submitted.
# Set AWS_ENDPOINT_URL=http://localhost:4566 in .env for LocalStack.
.PHONY: schema-register
schema-register:
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@rm -rf .tmp/glue-schemas
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" register \
	  --registry   "$(GLUE_REGISTRY_TENANT_NAME)" \
	  --schema-dir .tmp/glue-schemas

# schema-verify: fail if any of this service's 9 frozen PascalCase schema
# names (internal/eventschema.ByEventType, LLD §7.3.1) is missing from the
# Glue registry. Deliberately does NOT check for TenantCreated/TrialStarted —
# those are O&M's schemas on the shared registry, not this service's to
# verify. Requires GLUE_REGISTRY_TENANT_NAME and AWS credentials.
.PHONY: schema-verify
schema-verify:
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@missing=""; \
	for name in TrialTenantProvisioned TenantRealmReady TenantConverted DirectPaidSignup TrialExpired TrialReactivated TenantSuspended TenantOffboarded TenantReactivated; do \
	  if ! aws glue get-schema \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_TENANT_NAME),SchemaName=$$name" \
	      --region "$(AWS_REGION)" >/dev/null 2>&1; then \
	    missing="$$missing $$name"; \
	  fi; \
	done; \
	if [ -n "$$missing" ]; then \
	  echo "FAIL: missing Glue schemas in registry '$(GLUE_REGISTRY_TENANT_NAME)':$$missing"; \
	  echo "     run 'make schema-register' to create them"; \
	  exit 1; \
	fi; \
	echo "OK: all 9 iam-realm-provisioner schemas present in registry '$(GLUE_REGISTRY_TENANT_NAME)'"

# schema-prune: dry-run scan for orphaned Glue schemas (exist in Glue, not in
# this repo). WARNING: the shared iam-tenant-events registry will always
# list O&M's TenantCreated/TrialStarted as "orphaned" by this definition —
# they are not; ignore those two names in the output. Pass EXECUTE=true to
# archive and delete, but see docs/runbook-schema-registry.md first — CI
# (schema-prune.yml) hard-blocks --execute while either of those two names
# is present in the registry, since the underlying platform-schemagov CLI
# has no --exclude flag and prune --execute deletes every candidate it finds.
# Requires GLUE_REGISTRY_TENANT_NAME and AWS credentials.
.PHONY: schema-prune
schema-prune:
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	docker run --rm \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  "$(SCHEMA_GOV_IMAGE)" prune \
	  --registry   "$(GLUE_REGISTRY_TENANT_NAME)" \
	  $(if $(filter true,$(EXECUTE)),--execute,)

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
# SWAGGER
# -----------------------------

# swag: generate Swagger JSON/YAML from handler annotations into docs/swagger/.
# Re-run whenever annotations change. The output is checked in to the repo.
.PHONY: swag
swag:
	@echo "Generating Swagger docs..."
	$(GO) run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
	  -g swagger_info.go \
	  -d cmd/server,internal/adapter/inbound/http \
	  --output docs/swagger \
	  --parseDependency \
	  --parseInternal
	@echo "Swagger docs written to docs/swagger/"

.PHONY: swag-check
swag-check:
	bash .github/scripts/check-swagger-stale.sh

# -----------------------------
# CLEAN
# -----------------------------

.coverage:
	@mkdir -p .coverage

.PHONY: clean
clean:
	rm -rf bin .coverage
	rm -f coverage.out coverage.html
