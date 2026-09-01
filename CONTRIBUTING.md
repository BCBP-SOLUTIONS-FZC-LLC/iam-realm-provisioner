# Contributing

This is an internal IAM microservice for the XpertPMS platform. This guide covers development setup, how to extend the service, test requirements, and the PR process for `iam-realm-provisioner`.

## Prerequisites

- Go 1.26.6+ (matches `go.mod`)
- Docker (required for PostgreSQL + LocalStack (SNS/SQS) + Keycloak integration tests via `testcontainers-go`, and for local dev via `make docker-up`)
- `golangci-lint` is managed as a Go tool (`go tool golangci-lint`) — no separate install needed
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (private module access; also `GONOSUMDB` for the same prefix)
- SSH key registered with the BCBP org so `go mod download` can fetch `platform-events`, `platform-gincommon`, `platform-pgcommon`

## Development setup

```bash
git clone https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner
cd iam-realm-provisioner
make setup       # copies .env-example → .env
make tidy        # go mod tidy
make docker-up   # start PostgreSQL + LocalStack + Keycloak
make migrate-up  # apply pending migrations (the server also self-migrates at startup)
make lint        # verify linter passes
make test        # run all tests (requires Docker)
make run         # start the server on :8080 (metrics on :9090)
```

To run only unit tests (no Docker required):

```bash
make test-unit
```

Host ports are deliberately non-default so this stack can run alongside sibling IAM services: Postgres `5544`, LocalStack `4571`, Keycloak `8091` (see `docker-compose.yml`).

## Project layout

```
cmd/
  server/            ← Composition root — wiring, pool setup, middleware, SQS consumer loop, outbox runner, graceful shutdown
  reconciler/        ← Single binary dispatched via --job=<name>; drives 6 K8s CronJobs (RP-C1..RP-C4 + 2 retention jobs)
internal/
  core/domain/       ← Entities, value objects, error taxonomy (§17), event payload structs — no external deps
  core/port/         ← Interfaces required by the core: KeycloakAdmin (the RP-INV-1 seam), repositories,
                       IdpValidationRepository, ReconciliationRepository, IdempotencyStore, ProcessedEventsStore,
                       SubscriptionStatusReader (RP-C3 — no adapter wired yet, see CHANGELOG "Known gaps")
  core/service/      ← Use cases: ProvisioningService, LifecycleService, IdpService, MfaService, RealmConfigService
  adapter/inbound/
    http/            ← Gin handlers for RP-1..RP-16, DTOs, middleware
    consumer/        ← 3 SQS consumers (membership-realm-q, tenant-realm-q, billing-realm-q)
  adapter/outbound/
    keycloak/        ← gocloak-backed KeycloakAdmin implementation — the ONLY package permitted to import
                       Nerzal/gocloak (RP-INV-1, enforced by .github/scripts/arch-lint.sh)
    postgres/        ← Repository implementations + golang-migrate SQL migrations
    eventbus/        ← Outbox-backed publisher + ValidatingCodec (JSON Schema draft-07 at enqueue time)
    openbao/         ← KV v2 client for IdP secrets (Kubernetes auth), §12 / RP-D10
    orgmembership/   ← HTTP client for O&M (I-8 residual-MFA-requirement check)
    s3/              ← Realm export target on offboarding (§15.2)
    metrics/         ← iam_realm_provisioner_* Prometheus counters/histograms/gauges
  eventschema/        ← Embedded JSON Schema files (one per produced event) + ByEventType map
cmd/reconciler/jobs/  ← trial_expiry, trial_hard_delete, subscription_lapse, realm_config_reconcile,
                       outbox_prune, processed_events_prune
pkg/requestctx/       ← Typed request context
api/asyncapi.yaml     ← AsyncAPI 3.0 — the iam-tenant-events channel (9 published messages) + 3 inbound SQS channels
deploy/               ← Helm chart, AWS IAM policy, OpenBao policy, Prometheus alerts, Keycloak realm template
test/                 ← unit/, postgres/ (RLS, tagged `integration`), integration/ (Keycloak+SQS, tagged `integration`), e2e/ (tagged `e2e`)
```

**Dependency rule:** `domain` ← `port` ← `service` ← `adapter` ← `cmd`. Nothing in `core/` may import an adapter package, Gin, pgx, gocloak, or an AWS SDK. `core/service` may additionally import `pkg/requestctx` and the `metrics` package (a cross-cutting observability leaf, not a "real" adapter). Enforced in CI by `go-arch-lint` (`.go-arch-lint.yml`) plus two custom greps in `.github/scripts/arch-lint.sh` for the two invariants below.

For layering details and the request-flow diagrams, see [ARCHITECTURE.md](ARCHITECTURE.md).

## The two non-negotiable invariants

- **RP-INV-1 (single Keycloak writer).** Every mutating call to the Keycloak Admin API goes through `internal/adapter/outbound/keycloak/` via the `KeycloakAdmin` port. No other package may import `Nerzal/gocloak` — CI greps for this and fails the build on a violation. If you need a new Keycloak operation, add it to the `KeycloakAdmin` interface (`internal/core/port/keycloak_admin.go`) and implement it there, never inline in a handler or service.
- **RLS-6 (no session-scoped tenant GUC).** `app.tenant_id` must always be bound transaction-locally via `pgcommon`'s `GUCSetFromContext`/`WithGUCSet` (`SELECT set_config('app.tenant_id', $1, true)` inside a transaction). A session-scoped `SET app.tenant_id` would leak across tenants under PgBouncer connection reuse. CI greps for any non-`SET LOCAL` write to this GUC and fails the build.

## Extending the service

### Adding a new HTTP endpoint

1. Add the handler in `internal/adapter/inbound/http/`, wire the route in `cmd/server/main.go`.
2. If the endpoint mutates state, honor the `Idempotency-Key` header via `IdempotencyStore`/`provisioning_operations` (§9.2) and follow the Keycloak-effect-first-then-local-commit ordering (§9.1): the Keycloak mutation happens before any DB transaction opens; only on success does the local row + outbox event commit atomically.
3. Add unit tests in `test/unit/`; add HTTP-layer tests alongside the handler package if white-box coverage is needed (see `TEST_INTERNAL_PKGS` in the Makefile).
4. Update the endpoint catalogue in `README.md` and, if it's a new produced event, `api/asyncapi.yaml` + `internal/eventschema/`.

### Adding a new domain event type

1. Add the payload struct to `internal/core/domain/event.go` and the frozen `type` constant.
2. Build the event in the relevant service method and enqueue it via the outbox inside the same transaction as the business write (never "publish first, commit later").
3. Add the message to `api/asyncapi.yaml`'s `iamTenantEvents` channel and add the matching JSON Schema file under `internal/eventschema/`, registering it in `ByEventType`.
4. Run `go test ./internal/eventschema/... -run TestAsyncAPIVsSchemaDrift` — this is this service's CI-enforced drift gate between the AsyncAPI spec and the embedded schemas; it must pass before merge.
5. §23 names are frozen at v1.0 sign-off — do not rename an existing event type or field; add a new versioned event instead if the meaning changes.

### Adding a new Keycloak admin operation

1. Add the method to the `KeycloakAdmin` interface (`internal/core/port/keycloak_admin.go`).
2. Implement it in `internal/adapter/outbound/keycloak/` only. Never call gocloak from a service or handler directly.
3. If the operation is part of a multi-step sequence that can fail partway (e.g. realm creation followed by client registration), ensure a failure triggers compensating teardown (§9.3) before returning the error.

### Adding a new repository migration

1. Add `NNNNNN_description.up.sql` and `.down.sql` to `internal/adapter/outbound/postgres/migrations/`. Numbering is monotonic — never reuse or reorder.
2. If the table is tenant-scoped, it must ship with `ENABLE ROW LEVEL SECURITY` **and** `FORCE ROW LEVEL SECURITY`, plus a `USING (tenant_id = current_setting('app.tenant_id')::uuid)` policy (or equivalent), and a matching case added to `test/postgres/rls_test.go`'s coverage.
3. Verify with `make docker-up && make migrate-up && make run` — the server also self-migrates at startup via `platform-pgcommon`'s migration runner.

### Adding a new environment variable

1. Add it to `.env-example` with an inline comment explaining purpose and accepted values.
2. Add validation in the relevant `cmd/` `main.go` if it's required at startup.
3. Update the Environment variables table in `README.md`.

## Testing requirements

| Layer | Location | Docker | Notes |
|-------|----------|--------|-------|
| Unit | `test/unit/` + white-box `_test.go` files listed in `TEST_INTERNAL_PKGS` (Makefile) | No | Mock ports via `testify/mock` |
| PostgreSQL / RLS | `test/postgres/` (`-tags=integration`) | Yes | Real Postgres via testcontainers; the 5 canonical §14.5 RLS cases plus extras |
| Integration | `test/integration/` (`-tags=integration`) | Yes | Real Keycloak 26 + Postgres via testcontainers |
| E2E | `test/e2e/` (`-tags=e2e`) | Yes | Full lifecycle flows against the app's own test harness |

All unit tests must pass without Docker (`make test-unit`). Run the race detector before submitting a PR:

```bash
make race
```

### Canonical test cases

Do not break these without a design conversation:

- **`test/postgres/rls_test.go::TestRLS_Case5_NoGUCLeakageAcrossPooledConnection`** — verifies RLS-6 under `PGBouncerMode: true` (transaction-scoped GUC injection resets at commit; session-scoped injection does not clear on a no-GUC checkout, which is exactly the leak this guards against).
- **`test/integration/keycloak_test.go::TestKeycloakAdmin_TenantIDAttribute_PersistsAndIsQueryable`** — regression test for Keycloak's declarative User Profile silently dropping unmanaged user attributes; confirms `unmanagedAttributePolicy=ENABLED` is set on every realm this service creates.
- **`internal/eventschema/asyncapi_drift_test.go::TestAsyncAPIVsSchemaDrift`** — the AsyncAPI-vs-embedded-schema drift gate described above.

Run a single test:

```bash
go test ./test/unit/... -run TestSomeName -v
go test ./test/postgres/... -tags=integration -run TestRLS_Case5_NoGUCLeakageAcrossPooledConnection -v
go test ./test/integration/... -tags=integration -run TestKeycloakAdmin_TenantIDAttribute_PersistsAndIsQueryable -v
```

### Coverage gate

CI enforces a 70% global statement-coverage gate on the merged `coverage.out` (unit + postgres + integration suites, `./internal/...` + `./pkg/...`). See `.github/scripts/coverage-gate.sh`.

```bash
make cover-func   # per-function summary in terminal
make cover        # HTML report
```

## Linting

```bash
make lint       # golangci-lint (.golangci.yml)
make arch-lint  # go-arch-lint + RP-INV-1 + RLS-6 greps (.go-arch-lint.yml, .github/scripts/arch-lint.sh)
```

## Guardrails and forbidden patterns

- **Never call the Keycloak Admin API outside `internal/adapter/outbound/keycloak/`.** See RP-INV-1 above. CI enforces this.
- **Never write a session-scoped `SET app.tenant_id`.** See RLS-6 above. CI enforces this.
- **Never persist secret material to Postgres.** Realm passwords/seeds live only in Keycloak; SAML/OIDC IdP client secrets live only in OpenBao at `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>` (frozen, RP-D10). Do not widen this path's scope.
- **Never skip the idempotency ledger on a mutating endpoint.** A caller retry with the same `Idempotency-Key` must replay the stored result, not re-execute the mutation.
- **Never commit `.env`.** It's in `.gitignore`.

## Documentation update checklist

| What changed | Documents to update |
|---|---|
| New HTTP endpoint | `cmd/server/main.go` routing · `README.md` API overview table |
| New domain event type | `api/asyncapi.yaml` · `internal/eventschema/` (run the drift test) · `README.md` event table |
| New environment variable | `.env-example` (inline comment) · `README.md` Environment variables table |
| New repository migration | `CHANGELOG.md` `[Unreleased]` · RLS test coverage if tenant-scoped |
| Changed request/write flow | `ARCHITECTURE.md` |
| New CronJob or metric | `deploy/monitoring/app-alerts.yml` if it needs an alert · `deploy/helm/` if a new K8s workload is added |

## Branch naming

- `feat/<short-topic>` — new capability
- `fix/<short-topic>` — bug fix
- `docs/<short-topic>` — documentation-only change
- `chore/<short-topic>` — dependencies, tooling, CI, refactors with no behavioural change

Base new branches off the latest `main`.

## Pull request checklist

See [`.github/pull_request_template.md`](.github/pull_request_template.md) — it is applied automatically when you open a PR and covers code quality, event contract, RP-INV-1/secret-handling, RLS, idempotency, migrations, security, and documentation. At minimum, before opening a PR:

```bash
make ci   # tidy + fmt-check + vet + lint + arch-lint + test-ci + build — mirrors CI exactly
make race # zero data races required
```

## Commit style

Keep messages concise; describe the *why*. Reference the LLD section or invariant ID when it clarifies the change:

```
fix(keycloak): invalidate cached admin token after realm creation

Keycloak grants the create-realm principal admin on realms it creates via
a role-mapping added AFTER CreateRealm returns; a token minted before the
realm existed 403s on that realm's own admin endpoints. (§10.2, RP-INV-1.)
```

Use a short imperative subject line (≤ 72 chars):

```
feat(consumer): add billing-realm-q TenantReactivated handler (§8.11)
fix(rls): pin PGBouncerMode=true in Case 5 test to exercise transaction-scoped GUC reset
docs: update Environment variables table for BAO_IDP_PATH_PREFIX
```

## Deployment

Releases are handled via the CI/CD pipeline (`.github/workflows/ci.yml`):

1. `validate-test` (race detector + coverage gate + arch-lint + e2e) and `validate-quality` (fmt/tidy/vet/lint/vuln/mod-verify/Dockerfile checks) run in parallel with `build-image`.
2. A single Docker image is built (`linux/amd64`, distroless static-debian12:nonroot) carrying both `iam-realm-provisioner-server` and `iam-realm-provisioner-reconciler`; Trivy CVE scan and smoke tests run against it.
3. On push to `main`, the image is pushed to GHCR and signed with Cosign keyless signing (Sigstore OIDC).
4. The Helm chart in `deploy/helm/realm-provisioner/` is applied by the platform CD pipeline — one Deployment (the server) plus 6 CronJobs (the reconciler jobs), all dispatched from the same image with a `command` override.

Notify the platform team on Slack if a migration or breaking env-var change requires coordinated rollout — especially anything touching the OpenBao path convention or the Keycloak admin credential.
