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
# cmd/server and cmd/reconciler each carry one integration-tagged,
# Postgres-only composition-root test (main_integration_test.go, plus
# cmd/server's exporters_integration_test.go) exercising main()/run() end
# to end against a real testcontainers Postgres — no Keycloak needed, so
# they belong in the lighter "postgres" tier, not test-integration's
# heavier Postgres+Keycloak suite.
TEST_POSTGRES_PKGS := ./test/postgres/... ./cmd/server/... ./cmd/reconciler/... ./cmd/migrate/...
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
                      ./internal/adapter/inbound/sqspipeline/... \
                      ./internal/adapter/outbound/eventbus/... \
                      ./internal/adapter/outbound/postgres/... \
                      ./internal/adapter/outbound/keycloak/... \
                      ./internal/adapter/outbound/openbao/... \
                      ./internal/adapter/outbound/orgmembership/... \
                      ./internal/adapter/outbound/tokenservice/... \
                      ./internal/adapter/outbound/auditlog/... \
                      ./internal/adapter/outbound/httpx/... \
                      ./internal/adapter/outbound/s3/... \
                      ./internal/adapter/outbound/metrics/... \
                      ./internal/core/service/... \
                      ./internal/core/domain/... \
                      ./internal/core/port/... \
                      ./internal/eventschema/... \
                      ./pkg/...

COVER_PKG_LIST := $(shell $(GO) list ./cmd/... ./internal/... ./pkg/... 2>/dev/null | tr '\n' ',' | sed 's/,$$//')

# Every build tag a test file here declares (integration: test/integration + test/postgres, e2e: test/e2e, load: test/load). vet and lint run a
# second pass with all of them — CI's quality gate calls `make vet` /
# `make lint`, so without it the tagged test files were never checked.
ALL_TEST_TAGS := integration,e2e,load

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
	@echo "  make vet              - go vet (default build + every test build tag)"
	@echo "  make lint             - run golangci-lint (default build + every test build tag)"
	@echo "  make arch-lint        - run go-arch-lint (dependency-rule enforcement, §3.4)"
	@echo "  make metrics-registry-lint - no removed metric names in rules/HPA; proposed-metric alerts labelled metric_status: proposed"
	@echo "  make metric-naming-lint - enforce Platform Observability Standard naming/suffix/label/registry rules"
	@echo "  make metrics-lint     - platform-gincommon metricslint (real scrape + refs + inventory drift)"
	@echo "  make metrics-inventory - regenerate docs/observability/metric-registry.md"
	@echo "  make test             - unit + postgres + integration tests (requires Docker)"
	@echo "  make test-ci          - test with race detector + coverage (used in CI)"
	@echo "  make test-unit        - unit tests only (no Docker required)"
	@echo "  make test-postgres    - Postgres + RLS tests + cmd/ composition roots against real Postgres (requires Docker)"
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
	@echo "  make build            - compile server, reconciler and migrate to bin/"
	@echo "  make cover            - coverage HTML report"
	@echo "  make cover-func       - coverage summary by function"
	@echo "  make helm-check       - helm lint + template (4 value combos) + kubeconform (mirrors validate-helm.yml)"
	@echo "  make ci               - tidy + fmt-check + vet + lint + arch-lint + metrics-registry-lint + metric-naming-lint + metrics-lint + test-ci + build"
	@echo "  make docker-up        - start Postgres + Floci + Floci UI + Keycloak + OpenBao"
	@echo "  make docker-down      - stop local containers"
	@echo "  make migrate-up       - apply pending domain migrations against MIGRATION_DATABASE_URL"
	@echo "  make migrate-down     - roll back one domain migration"
	@echo "  make migrate-create   - scaffold a new migration pair: make migrate-create NAME=add_x"
	@echo "  make mod-verify       - go mod verify"
	@echo "  make vuln-check       - govulncheck ./..."
	@echo "  make clean            - remove build artefacts"
	@echo ""
	@echo "Schema governance (platform-schemagov 0.4, shared iam-tenant-events registry):"
	@echo "  make schema-pull      - pull the platform-schemagov Docker image"
	@echo "  make extract-schemas  - derive internal/eventschema/*.json from api/asyncapi.yaml"
	@echo "  make schema-validate  - validate AsyncAPI + event schemas — 8 passes (no AWS required)"
	@echo "  make schema-diff      - diff two schema files: CURRENT=<path> PROPOSED=<path>"
	@echo "  make schema-register  - register this service's 10 event schemas to Glue (requires AWS, or Floci for local dev)"
	@echo "  make schema-verify    - pre-deploy check: each produced schema definition registered + AVAILABLE (requires AWS)"
	@echo "  make schema-prune     - dry-run: list orphaned Glue schemas (requires AWS; excludes O&M's TenantCreated/TrialStarted)"
	@echo ""
	
	@echo "Swagger:"
	@echo "  make swag             - generate Swagger docs from annotations (output: docs/swagger/)"
	@echo " 
	
	
	
	
	
	
	
	
	
	
	
	
	
	
	
	    - fail if Swagger regeneration changes docs/swagger/"

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
	$(GO) vet -tags=$(ALL_TEST_TAGS) ./...

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
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

# -----------------------------
# LINT
# -----------------------------

.PHONY: lint
lint:
	@echo "Running linter..."
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags=$(ALL_TEST_TAGS) ./...

.PHONY: arch-lint
arch-lint:
	bash .github/scripts/arch-lint.sh

.PHONY: metrics-registry-lint
metrics-registry-lint:
	bash .github/scripts/metrics-registry-lint.sh

# metrics-lint: platform-gincommon's metricslint over a real /metrics scrape
# and the alert/dashboard/runbook references (same script CI runs).
.PHONY: metrics-lint
metrics-lint:
	bash .github/scripts/metricslint.sh

# metrics-inventory: regenerate docs/observability/metric-registry.md from the
# pinned registry and a real scrape (metrics-lint fails when it is stale).
.PHONY: metrics-inventory
metrics-inventory:
	METRICS_INVENTORY_WRITE=1 bash .github/scripts/metricslint.sh

.PHONY: metric-naming-lint
metric-naming-lint:
	bash .github/scripts/check-metric-naming.sh

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
	$(GO) build -trimpath -ldflags "-s -w -X main.buildVersion=$(BUILD_VERSION)" -o bin/migrate ./cmd/migrate
	@echo "Verifying library packages compile..."
	$(GO) build ./internal/... ./pkg/...

# -----------------------------
# MIGRATIONS (standalone golang-migrate CLI — the server binary also
# self-migrates at startup via pgcommon/pkg/migrate; these targets are for
# local dev/CI convenience only, §4/§19)
# -----------------------------

MIGRATIONS_DIR := internal/adapter/outbound/postgres/migrations

.PHONY: migrate-up
# MIGRATION_DATABASE_URL is the name .env, Helm and cmd/migrate use;
# DATABASE_MIGRATION_URL is still accepted for existing shells.
MIGRATION_DSN = $(or $(MIGRATION_DATABASE_URL),$(DATABASE_MIGRATION_URL))

migrate-up:
	@test -n "$(MIGRATION_DSN)" || { echo "MIGRATION_DATABASE_URL not set"; exit 1; }
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
	  migrate/migrate -path=/migrations -database "$(MIGRATION_DSN)" up

.PHONY: migrate-down
migrate-down:
	@test -n "$(MIGRATION_DSN)" || { echo "MIGRATION_DATABASE_URL not set"; exit 1; }
	docker run --rm -v $(CURDIR)/$(MIGRATIONS_DIR):/migrations --network host \
	  migrate/migrate -path=/migrations -database "$(MIGRATION_DSN)" down 1

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
	@echo "Starting local PostgreSQL + Floci + Floci UI + Keycloak + OpenBao..."
	docker compose up -d postgres floci floci-ui keycloak openbao

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

# schema-pull: pull the platform-schemagov Docker image. --platform=linux/amd64
# is required — the image ships amd64-only (no arm64/v8 manifest), so a bare
# `docker pull` on Apple Silicon fails with "no matching manifest"; every
# other SCHEMA_GOV_IMAGE invocation below needs the same flag on `docker run`.
.PHONY: schema-pull
schema-pull:
	docker pull --platform=linux/amd64 "$(SCHEMA_GOV_IMAGE)"

# extract-schemas: derive internal/eventschema/*.json from api/asyncapi.yaml.
# api/asyncapi.yaml is the single source of truth for event payload schemas.
# Re-run whenever asyncapi.yaml changes; commit the updated JSON files alongside.
# The JSON files are read by schema-gov validate --schema-dir internal/eventschema.
.PHONY: extract-schemas
extract-schemas:
	@echo "Extracting event schemas from api/asyncapi.yaml..."
	docker run --rm --platform=linux/amd64 \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" extract \
	  --asyncapi   api/asyncapi.yaml \
	  --schema-dir internal/eventschema
	@echo "Done. Run 'git add internal/eventschema/' to stage the changes."

# schema-validate: validate AsyncAPI spec + event schemas — 8 passes:
# (1) structure, (2) draft-07, (3) enum drift, (4) lifecycle annotations,
# (5) open-schema guard, (6) consumer strict-mode, (7) coverage, (8) AsyncAPI structure.
# No AWS credentials needed. Pass 4 only warns (does not fail) on schemas with
# no x-lifecycle annotation — this service's 10 events are frozen (LLD §25)
# and deliberately carry none yet (see ARCHITECTURE.md).
.PHONY: schema-validate
schema-validate: extract-schemas
	docker run --rm --platform=linux/amd64 \
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
	docker run --rm --platform=linux/amd64 \
	  -v "$(CURDIR)":/workspace \
	  "$(SCHEMA_GOV_IMAGE)" diff \
	  --current     "$(CURRENT)" \
	  --proposed    "$(PROPOSED)" \
	  --schema-name "$(or $(SCHEMA_NAME),$(notdir $(basename $(PROPOSED))))"

# schema-register: register this service's 10 event schemas to Glue (requires
# AWS credentials, or Floci for local dev — already registered automatically
# by scripts/init-floci.sh on `make docker-up`, so this target is mainly for
# CI/real-AWS use). Only ever registers the files present under
# internal/eventschema/ — never touches O&M's TenantCreated/TrialStarted,
# which have no file here and are therefore never submitted.
.PHONY: schema-register
schema-register:
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@rm -rf .tmp/glue-schemas
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas
	docker run --rm --platform=linux/amd64 \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" register \
	  --registry   "$(GLUE_REGISTRY_TENANT_NAME)" \
	  --schema-dir .tmp/glue-schemas

# schema-verify: fail unless every produced schema has an AVAILABLE version
# whose definition matches this checkout's file — the exact lookup GlueCodec
# does at startup (GetSchemaByDefinition, compact + ASCII-escaped like
# schema-gov register uploads it). Reads the same PascalCase staged copy
# schema-register uploads, so it covers exactly the produced schemas. Surfaces
# "pod would CrashLoop on NewGlueCodec" pre-deploy, which a name-only
# get-schema check cannot. Requires GLUE_REGISTRY_TENANT_NAME, AWS credentials (or
# predeploy-check: run scripts/pre-deploy-check.sh to validate all 12
# production go-live prerequisites before the first deploy.
# Requires: aws CLI, kubectl, gh CLI, BAO_ADDR, and the environment vars
# documented in .claude/operations.md.
.PHONY: predeploy-check
predeploy-check:
	@bash scripts/pre-deploy-check.sh

# AWS_ENDPOINT_URL for floci) and python3.
.PHONY: schema-verify
schema-verify:
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@rm -rf .tmp/glue-schemas-verify
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas-verify >/dev/null
	@failed=""; count=0; \
	for file in .tmp/glue-schemas-verify/*.json; do \
	  name=$$(basename "$$file" .json); count=$$((count + 1)); \
	  def=$$(python3 -c 'import json,sys; sys.stdout.write(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")))' "$$file"); \
	  status=$$(aws glue get-schema-by-definition \
	      --schema-id "RegistryName=$(GLUE_REGISTRY_TENANT_NAME),SchemaName=$$name" \
	      --schema-definition "$$def" \
	      --region "$(AWS_REGION)" --query Status --output text 2>/dev/null); \
	  if [ "$$status" != "AVAILABLE" ]; then \
	    failed="$$failed $$name($${status:-not-registered})"; \
	  fi; \
	done; \
	rm -rf .tmp/glue-schemas-verify; \
	if [ -n "$$failed" ]; then \
	  echo "FAIL: this checkout's schema definition is not registered+AVAILABLE in '$(GLUE_REGISTRY_TENANT_NAME)':$$failed"; \
	  echo "     run 'make schema-register' (or wait for schema-registry.yml) to register it"; \
	  exit 1; \
	fi; \
	echo "OK: all $$count iam-realm-provisioner schema definitions registered and AVAILABLE in '$(GLUE_REGISTRY_TENANT_NAME)'"

# schema-prune: dry-run scan for orphaned Glue schemas (exist in Glue, not in
# this repo). WARNING: the shared iam-tenant-events registry will always
# list O&M's TenantCreated/TrialStarted as "orphaned" by this definition —
# they are not; ignore those two names in the output. Pass EXECUTE=true to
# archive and delete, but see docs/runbook-schema-registry.md first — CI
# (schema-prune.yml) hard-blocks --execute while either of those two names
# is present in the registry, since the underlying platform-schemagov CLI
# has no --exclude flag and prune --execute deletes every candidate it finds.
# Requires GLUE_REGISTRY_TENANT_NAME and AWS credentials.
# schema-prune compares Glue against the PascalCase staged copy: prune treats a
# schema as orphaned when no FILE STEM in --schema-dir matches it, and the
# committed files are snake_case — against them (or the default dir) it lists
# every live schema as an orphan, which EXECUTE=true would delete.
.PHONY: schema-prune
schema-prune:
	@test -n "$(GLUE_REGISTRY_TENANT_NAME)" || { \
	  echo "GLUE_REGISTRY_TENANT_NAME is not set — add it to .env or pass on the command line"; \
	  exit 1; \
	}
	@if [ "$(EXECUTE)" = "true" ]; then \
	  echo "Refusing EXECUTE=true: $(GLUE_REGISTRY_TENANT_NAME) is shared with iam-org-membership, whose"; \
	  echo "TenantCreated/TrialStarted have no file here, so prune --execute would delete them. Use"; \
	  echo "schema-prune.yml, whose guard step blocks that. Dry-run (no EXECUTE) is safe."; \
	  exit 1; \
	fi
	@rm -rf .tmp/glue-schemas
	@bash .github/scripts/stage-produced-event-schemas.sh .tmp/glue-schemas >/dev/null
	docker run --rm --platform=linux/amd64 \
	  -v "$(CURDIR)":/workspace \
	  -e AWS_ACCESS_KEY_ID \
	  -e AWS_SECRET_ACCESS_KEY \
	  -e AWS_SESSION_TOKEN \
	  -e AWS_REGION="$(AWS_REGION)" \
	  -e AWS_ENDPOINT_URL="$(AWS_ENDPOINT_URL)" \
	  "$(SCHEMA_GOV_IMAGE)" prune \
	  --registry   "$(GLUE_REGISTRY_TENANT_NAME)" \
	  --schema-dir .tmp/glue-schemas \
	  $(if $(filter true,$(EXECUTE)),--execute,)

# -----------------------------
# CI
# -----------------------------

# helm-check: lint, render all four values combos, and kubeconform-validate
# rendered manifests. Mirrors .github/workflows/validate-helm.yml exactly.
# Requires helm + kubeconform (installed automatically on first run via
# go install if absent). Does not require Docker or a live cluster.
.PHONY: helm-check
helm-check:
	bash .github/scripts/helm-check.sh

.PHONY: ci
ci: tidy fmt-check vet lint arch-lint metrics-registry-lint metric-naming-lint metrics-lint test-ci build

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
