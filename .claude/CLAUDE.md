# CLAUDE.md

This file provides guidance to Claude Code when working with the **Realm Provisioner Service** (`iam-realm-provisioner`), a Go microservice in the IAM subsystem (Tender Management SaaS platform). Refines the IAM HLD; LLD v1.6 Approved (`docs/lld/iam-lld-realm-provisioner-service.md`, condensed reference at `docs/lld/IMPLEMENTATION_REFERENCE.md`), owner database `provisioner` on shared RDS PostgreSQL. Where LLD and HLD disagree, HLD is authoritative.

## What This Repo Is

`iam-realm-provisioner` is a **private Go service** (module: `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner`) that is the **single writer to the Keycloak Admin API** (RP-INV-1) and the system of record for tenant identity lifecycle in the IAM platform. It provisions and tears down Keycloak realms and users, migrates trial tenants into dedicated realms, configures per-tenant SAML/OIDC identity providers (secrets held only in OpenBao, never Postgres), and drives the tenant lifecycle state machine (trial → active → suspended → offboarded) via six scheduled reconciler jobs. Org & Membership owns the organizational/business model and calls this service over the mesh; this service owns Keycloak realm/user/IdP *administration* and the small amount of local state needed to make that administration safe, idempotent, and replayable.

**Key responsibilities (sole writer for):**
- **`tenant_realms`** — one row per tenant: realm name/strategy (`shared`|`dedicated`), Keycloak shard, lifecycle status (`provisioning`|`active`|`disabled`|`suspended`|`exported`|`deleted`), trial/conversion/disable/export timestamps, `record_version`.
- **`keycloak_idp_configs`** — per-tenant SAML/OIDC IdP configuration, **non-secret** params only; the secret itself lives exclusively in OpenBao.
- **`keycloak_idp_config_validations`** — append-only §11.6 region-portability validation run history + admin acknowledgements.
- **`realm_config_reconciliations`** — RP-C4's retry ledger for realm-config settings that failed to apply inline (local-intent-first, RP-INV-6).
- **`provisioning_operations`** — the idempotency ledger, keyed by the caller-supplied `Idempotency-Key` header (or an SQS envelope ID for consumer-driven operations).
- **The Keycloak Admin API itself** — every mutation against Keycloak in the entire IAM subsystem goes through this service's `KeycloakAdmin` port, implemented by exactly one adapter using `Nerzal/gocloak`. No other IAM service, and no other package in this repo, may call Keycloak's admin endpoints directly — enforced in CI by `.github/scripts/arch-lint.sh` grepping every Go file for a stray `gocloak` import.

**Does NOT own:** the organizational/business model — tenants-as-business-entities, memberships, department/role grants, plan/subscription state (Org & Membership); credentials validation at request time or JWT issuance (Keycloak itself, at runtime); delegation, tender ACL overlays, or group→role mapping config (their own sibling services); any per-user table (there is none — Keycloak is the sole store of user identity/credentials); secret material of any kind in Postgres (realm passwords and trial-user seed credentials live only in Keycloak; IdP client secrets live only in OpenBao at `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`).

**Two ordering disciplines, used per-flow (§9.1):**
1. **Keycloak-effect-first, then local commit** — the default for every provisioning/migration/IdP/MFA/realm-config-apply-success flow. The Keycloak mutation runs entirely outside any Postgres transaction; only on success does `RunInTx` commit the local row **and** enqueue the outbox event(s) atomically. A crash between the two is recovered by idempotent replay of the same `Idempotency-Key` — the Keycloak step becomes a no-op reconcile (already exists), then the local commit lands. A committed local row therefore always reflects a real Keycloak state, never the reverse.
2. **Local-intent-first, then reconcile** — used only for RP-8 realm-config when Keycloak is transiently unreachable: the desired value is recorded `pending` in `realm_config_reconciliations` and the caller gets `200 {applied:false, reconciling:true}` (not an error), and RP-C4 converges it later.

## Common Commands

```bash
make setup             # Copy .env-example -> .env + install .githooks/pre-commit (run once before anything else)
make install-hooks     # Install .githooks/pre-commit standalone (also run by make setup)
make tidy               # go mod tidy
make fmt                # go fmt ./...
make fmt-check          # Verify gofmt formatting without modifying files (mirrors CI)
make vet                # go vet ./...
make lint               # golangci-lint (via go tool golangci-lint)
make arch-lint          # go-arch-lint dependency-rule enforcement (§3.4) + RP-INV-1/RLS-6 greps
make mod-verify         # go mod verify (check module download integrity)
make vuln-check         # govulncheck ./internal/... ./pkg/...
make test               # Unit + postgres + integration tests in parallel (requires Docker; e2e is separate — make test-e2e)
make test-ci            # Unit + postgres + integration with -race + coverage (used in CI)
make test-unit          # Unit tests only (no Docker required)
make test-postgres      # Postgres + RLS integration tests (testcontainers-go)
make test-integration   # Cross-layer integration tests (SNS/SQS/Keycloak/OpenBao via testcontainers)
make test-e2e           # End-to-end tests
make test-load          # RP-3 migration load test (fake Keycloak, no Docker); LOAD_TEST_FULL=1 for N=500
make race               # All tests with -race flag
make run                # Run the server locally (kills APP_PORT first, sources .env)
make run-reconciler JOB=<name>  # Run one reconciler job locally, e.g. JOB=trial-expiry
make build              # Compile both binaries to bin/ (iam-realm-provisioner-server, iam-realm-provisioner-reconciler)
make cover              # Coverage HTML report
make cover-func         # Coverage summary by function
make ci                 # tidy + fmt-check + vet + lint + arch-lint + test-ci + build (full CI pipeline)
make docker-up          # Start Postgres + LocalStack + Keycloak + OpenBao (Docker required)
make docker-up-pro      # Standalone LocalStack Pro stack instead (docker-compose.pro.yml — Glue + KMS, same Postgres/Keycloak/OpenBao; requires LOCALSTACK_AUTH_TOKEN)
make docker-down        # Stop containers
make migrate-up         # Apply pending domain migrations against DATABASE_MIGRATION_URL (server also self-migrates at startup)
make migrate-down       # Roll back one domain migration
make migrate-create NAME=<x>  # Scaffold a new migration pair
make clean              # Remove bin/ artefacts and coverage files
make schema-validate    # platform-schemagov 8-pass AsyncAPI + event-schema validation (no AWS required)
make schema-register    # Register this service's 9 event schemas to the shared iam-tenant-events Glue registry
make schema-verify      # Pre-deploy check: fail if any of the 9 schemas is missing from Glue (requires AWS)
make schema-prune       # Dry-run: list orphaned Glue schemas (requires AWS; excludes O&M's TenantCreated/TrialStarted)
make swag               # Generate Swagger docs (docs/swagger/) from handler annotations — re-run + commit on change
make swag-check         # Fails if docs/swagger/ is stale relative to handler annotations (regenerate-and-diff, not yet wired into any workflow)
```

**Testcontainers note:** Postgres, Keycloak, and OpenBao integration tests spin up real containers via `testcontainers-go`. Each `test/postgres/*.go` test calls `t.Parallel()` and spins its own container, capped at `TEST_POSTGRES_PARALLEL` (default 4) rather than `GOMAXPROCS`-wide. Each `test/integration/*.go` Keycloak test also calls `t.Parallel()` and spins its own container — a JVM/Quarkus image, far heavier than Postgres — capped tighter at `TEST_INTEGRATION_PARALLEL` (default 2); left uncapped this defaults to `GOMAXPROCS` and OOM-kills containers on dev/CI boxes with <16GB of Docker memory. The OpenBao tests in the same package (`openbao_test.go`) share that cap and additionally drive a real Kubernetes-auth login by stubbing the TokenReview endpoint the container calls out to (no real Kubernetes cluster is available). Docker must be running for anything beyond `test-unit`.

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers. Two composition roots ship from one Docker image: `cmd/server/main.go` (HTTP API + 3 SQS consumers + outbox runner) and `cmd/reconciler/main.go` (CronJob dispatcher, `--job=<name>`). Full directory tree, shared library dependencies (`platform-gincommon` v1.3.0, `platform-events` v1.4.0, `platform-pgcommon` v1.3.0, `Nerzal/gocloak` v13.9.0), RoutingPublisher-vs-single-topic wiring, and `go-arch-lint` dependency rules (including the two CI-enforced invariants beyond the layering check — RP-INV-1 and RLS-6) are in **[`.claude/architecture.md`](architecture.md)**.

## Key Files to Know

- **`cmd/server/main.go`** / **`cmd/server/wiring.go`** — HTTP composition root: pool + GUC wiring (`GUCProvider = GUCSetFromContext`, transaction-local `app.tenant_id`), migrations (`outbox.ApplySchema` **before** the domain migration — the domain migration ALTERs `outbox_events.payload`), outbox runner, 3 SQS consumer loops, graceful shutdown. `/healthz` (pure liveness) and hand-rolled `/readyz` (Postgres + sys-Postgres + outbox + Keycloak `Ready()`) via `gincommon`. `/metrics` on its own dedicated port (`METRICS_PORT=9090`), never shared with the mesh-internal API.
- **`cmd/reconciler/main.go`** + **`cmd/reconciler/jobs/`** — CronJob dispatcher (`--job=<name>`), shared `jctx.Context` construction; 6 jobs total (RP-C1..RP-C4 + 2 operational prune jobs). `subscription_lapse.go` (RP-C3) is a two-phase suspend→offboard sweep against O&M's I-16 `GET /api/v1/internal/subscription-lapses`; when `ORG_MEMBERSHIP_BASE_URL` is unset the suspend phase logs a WARN and skips (the offboard phase for already-suspended `billing_lapse` tenants still runs unconditionally).
- **`internal/core/domain/*.go`** — entities, value objects, `ErrorCode` taxonomy (§17) with `Error{Code, Message, Details}` and `Status()` → HTTP code; `event.go` carries `Source="iam-realm-provisioner"`, `SystemActorID`, and every produced/consumed event-type constant + payload struct (§25 frozen name inventory). No external deps beyond `google/uuid`.
- **`internal/core/port/keycloak_admin.go`** — `KeycloakAdmin`, RP-INV-1's sole seam (~20 methods): realm CRUD, user CRUD + `MigrateUser` (cross-realm, `sub`-preserving), IdP CRUD, MFA credential reset, session revocation, realm export.
- **`internal/core/port/outbound_clients.go`**-equivalent — `port/subscription_status_reader.go` (`SubscriptionLapseReader`, RP-C3's suspend-phase trigger, backed by O&M's I-16), `port/openbao_client.go` (IdP secret KV read/write/delete), `port/org_membership_client.go` (O&M's I-8 residual-MFA-requirement read), `port/object_store.go` (`PutRealmExport` to S3). Both O&M dependencies are implemented by one adapter, `internal/adapter/outbound/orgmembership`.
- **`internal/core/service/provisioning_service.go`** — RP-1/2/3/4 (`ProvisionTrial`, `ProvisionDirect`, `Convert`, `CreateInvitedUser`, `RevertConversion`); the canonical Keycloak-effect-first flow, including compensating teardown (`bestEffortDeleteRealm`) when a later step in a multi-step Keycloak sequence fails.
- **`internal/core/service/lifecycle_service.go`** — RP-10/11/12/14/15/16 (`Reactivate`, `GetRealm`, `ExpireTrial`, `Suspend`, `DeleteUser`, `RevokeSessions`). `Reactivate` (RP-10) reverses **both** a billing-lapse suspension and an RP-14 operator suspension from one endpoint.
- **`internal/core/service/idp_service.go`** / **`idp_validator.go`** — RP-6/7/13 (`ConfigureIdp`, `RemoveIdp`, `PortabilityAudit`) and the §11.6 Region-Agnostic IdP Configuration Checklist (`ValidateIdpConfig`) — CRITICAL checks block with `400 idp_region_portability_failed`; WARN checks require a recorded acknowledgement or `422 idp_warnings_unacknowledged`.
- **`internal/core/service/realm_config_service.go`** — RP-8 (`ApplyRealmConfig`), the one place a client-facing `200` does not mean the Keycloak-side effect has landed yet (RP-INV-6 local-intent-first).
- **`internal/core/service/mfa_service.go`** — RP-9 (`ResetMFA`, fail-closed, no reconciler) + `membership-realm-q`'s `SyncRequiresMfa` (a **reconcile**, not an apply-a-delta — grant is unconditional assign-if-absent, revoke on `TenantRoleRevoked` only, after consulting O&M's residual-MFA-requirement check).
- **`internal/adapter/inbound/http/router.go`** — single source of truth for the route table (shared by `cmd/server/main.go` and the e2e test harness); all 16 endpoints live under `/api/v1/internal/*`, no public surface. Middleware chain: body-size cap → `TimeoutMiddleware(30s)` → `ObservabilityMiddlewares` → `ProtectedMiddlewares` → `GUCBridgeMiddleware` → `RequireJSONContentType` → `RequireSystemRole` (defense-in-depth; mesh mTLS + NetworkPolicy is primary) → per-route `RequireIdempotencyKey` / `RequireOperatorRole`.
- **`internal/adapter/inbound/consumer/`** — 3 SQS handlers: `membership-realm-q`, `tenant-realm-q`, `billing-realm-q`, each with EVT-style dedup via `processed_events`.
- **`internal/adapter/outbound/postgres/`** — repository impls + the single consolidated `migrations/000000_initial_schema.{up,down}.sql` (this service has never shipped a prior schema version).
- **`internal/adapter/outbound/keycloak/`** — `gocloak`-backed `KeycloakAdmin` impl, RP-INV-1's **only** implementation; wraps the SDK with a shared admin-token cache + single-flight refresh, bounded exponential-backoff retry on 5xx/timeout (429 honours `Retry-After`), and a client-side rate limiter (`KEYCLOAK_RATE_LIMIT_RPS`).
- **`internal/adapter/outbound/eventbus/`** — single-topic outbox publisher (`iam-tenant-events`) + Glue/Noop/Validating codecs.
- **`internal/adapter/outbound/openbao/`** — OpenBao KV v2 client, Kubernetes-auth (pod ServiceAccount token → OpenBao role → policy).
- **`internal/adapter/outbound/orgmembership/`** — HTTP client for O&M's I-8 residual-MFA check and I-16 subscription-lapse poll (`port.SubscriptionLapseReader`, RP-C3's suspend-phase trigger).
- **`internal/adapter/outbound/s3/`** — realm-export object store (offboarding, RP-EVT-4: realm is never deleted without a confirmed durable export).
- **`internal/adapter/outbound/httpx/`** — the one HTTP transport every outbound adapter builds on, injecting W3C `traceparent` (§11.3) so a provision → event → O&M projection stays one trace.
- **`internal/adapter/outbound/postgres/gauge_repository.go`** + **`cmd/server/exporters.go`** — `runBusinessExporters`, 4 goroutines on the BYPASSRLS `sysPool`: 3 DB-state gauges (`realm_active_gauge`, `realm_config_pending_gauge`, `template_drift`) plus the monotonic `rls_violations_total` counter backing the two RLS-6 alerts. The counter's exporter needs a Terraform-side grant this repo cannot itself provide — see `.claude/operations.md` §20.
- **`internal/eventschema/`** — embedded JSON Schema draft-07 files, one per produced event, plus `asyncapi_drift_test.go` — this repo's CI-enforceable substitute for a live Glue registry comparison: it asserts `api/asyncapi.yaml`'s channel message list and `eventschema.ByEventType`'s map keys name exactly the same 9 events.
- **`test/postgres/rls_test.go`** — canonical RLS test cases including the critical no-GUC-leakage-across-a-pooled-PgBouncer-backend case (RLS-6).

## Data Model

Database `provisioner` on shared RDS PostgreSQL (Multi-AZ, PgBouncer transaction pooling in production); **5 tenant-scoped tables** under `FORCE ROW LEVEL SECURITY` (`tenant_realms`, `keycloak_idp_configs`, `keycloak_idp_config_validations`, `realm_config_reconciliations`, `provisioning_operations` — the last is RLS-scoped, unlike a typical idempotency ledger, because its `result` payloads can embed tenant data), **3 RLS-exempt operational tables** (`outbox_events`, `processed_events`, `exporter_watermarks`), and **1 audit-only table with RLS deliberately disabled** (`rls_violation_log`, so its own writer function doesn't recurse into its own policy check). `record_version`-based optimistic locking on the two tables that carry it (`tenant_realms`, `keycloak_idp_configs`). Single consolidated migration `000000_initial_schema` (this service has never shipped a prior version). Full table catalogue, RLS invariants (Cases 1–5 + two structural tests), triggers, migration ordering, and the "data not stored here by design" list (no secrets, no per-user table, no tenant business-entity table) are in **[`.claude/database-schema.md`](database-schema.md)**.

## API, Caching & Events

One route prefix, `/api/v1/internal/*` — this service has almost no public surface; every route is mesh-internal (Signup BFF, Org & Membership, Billing via events, operator tooling). `/metrics` is served on a separate port, never on the main router. **16 endpoints** (RP-1..RP-16): 14 system-authenticated, 2 operator-only (RP-13 whole-estate IdP audit, RP-14 emergency suspend). Every mutating endpoint marked "required" enforces `RequireIdempotencyKey` against the `provisioning_operations` ledger — missing header is `400 missing_idempotency_key`, a reused key against an in-flight/failed operation is `409 operation_in_progress`, a reused key against a `succeeded` operation replays the stored result rather than re-executing. One SNS topic (`iam-tenant-events`, **not** a `RoutingPublisher` like some siblings) carries all **9 produced events**; three separate inbound SQS queues (`membership-realm-q`, `tenant-realm-q`, `billing-realm-q`), each with its own consumer and `-dlq` sibling (`maxReceiveCount=5`), dedup via `processed_events`. Full endpoint catalogue, error taxonomy, event catalogue (produced + consumed), and AsyncAPI/schema-drift mechanics are in **[`.claude/api-caching-events.md`](api-caching-events.md)**.

## Request Flows, Concurrency & GDPR

Cross-cutting discipline: Keycloak-effect-first, then local commit (§9.1, see above) — visible in every method in `provisioning_service.go` and `lifecycle_service.go`. Compensating teardown on multi-step Keycloak sequence failure (`ProvisionDirect`, `Convert` — `bestEffortDeleteRealm`, itself idempotent). Full flows for trial provisioning (RP-1), direct-paid provisioning (RP-2, with least-loaded shard placement from the **local** mirror), trial→dedicated conversion with user migration (RP-3, `sub`-preserving, TOTP copied, WebAuthn re-enrollment flagged), revert-conversion (RP-4, within a fixed window), IdP configuration under the §11.6 gate (RP-6/7), realm-config local-intent-first (RP-8/RP-C4), MFA reset + role-sync reconcile (RP-9 + `membership-realm-q`), reactivate (RP-10), trial expiry (RP-12/RP-C1), operator suspend (RP-14), idempotent user delete/session-revoke (RP-15/16), trial hard-delete (RP-C2), and subscription lapse suspend→offboard (RP-C3) are all in **[`.claude/request-flows.md`](request-flows.md)**, along with the idempotency-ledger, consumer-dedup, and GDPR/data-lifecycle mechanics (PII scope, erasure via RP-C1/RP-C2/RP-C3, retention schedule).

## Operations

Security (Keycloak admin role set — `create-realm` on master, `realm-admin` composite on managed realms; credential protection via OpenBao, never env/image/git; per-tenant IdP secrets, never a Postgres pointer; network isolation, mesh-only); observability (`iam_realm_provisioner_*`-prefixed metrics, tracing, structured logging, dashboards, alerts); configuration (all env vars, fail-fast checks, realm-template versioning + drift detection); deployment (topology, two workloads from one image, 6 CronJobs, Helm chart layout, CI/CD pipeline); testing strategy (unit/integration/contract/e2e/RLS/load); GDPR/data-lifecycle (minimal PII, erasure on trial hard-delete and offboarding, export-before-delete invariant); integration points with O&M/Billing/Signup BFF. Full detail in **[`.claude/operations.md`](operations.md)**.

## See Also

Detailed reference docs in `.claude/`:
- [`architecture.md`](architecture.md) — Clean Architecture directory tree, shared library integration, dep rules
- [`database-schema.md`](database-schema.md) — 9 tables (5 RLS-scoped + 3 exempt + 1 audit-only), enums, RLS invariants, triggers, migrations
- [`api-caching-events.md`](api-caching-events.md) — full endpoint catalogue (RP-1..RP-16), error taxonomy, event catalogue, AsyncAPI/drift mechanics
- [`request-flows.md`](request-flows.md) — Keycloak-effect-first discipline, compensating teardown, every RP-*/RP-C* flow, idempotency mechanics
- [`operations.md`](operations.md) — security, observability, configuration, CI/CD, testing, GDPR, integration points, open items

Full LLD (v1.6 Approved, `docs/lld/iam-lld-realm-provisioner-service.md`) and its condensed reference (`docs/lld/IMPLEMENTATION_REFERENCE.md`, §1 frozen name inventory through §24 operational runbooks), checked into this repo.

Supplementary docs in the repo root:
- **`README.md`** — mental model, API/event overview, onboarding
- **`ARCHITECTURE.md`** — detailed architecture narrative with diagrams
- **`docs/runbook.md`** — full operator procedures for the headline incident scenarios (§24)
