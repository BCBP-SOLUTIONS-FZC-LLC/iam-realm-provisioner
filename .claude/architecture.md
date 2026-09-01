# Architecture

Refines LLD §3, §6, §6.2. Clean Architecture / Ports-and-Adapters — dependencies point inward; `core/domain` imports nothing internal. `cmd/server/main.go` (HTTP API + 3 SQS consumers + outbox runner) and `cmd/reconciler/main.go` (CronJob dispatcher, `--job=<name>`) are the two composition roots; both ship from one Docker image (see Dockerfile).

## Package Layout

```
iam-realm-provisioner/
├── cmd/
│   ├── server/
│   │   ├── main.go                     # HTTP composition root: pool+GUC wiring, migrations, outbox runner, SQS consumer loop, graceful shutdown
│   │   └── wiring.go                   # DI helpers factored out of main.go
│   └── reconciler/
│       ├── main.go                     # CronJob dispatcher, --job=<name>, shared jctx.Context construction
│       ├── noop_publisher.go           # no-op EventPublisher for a dry-run/local invocation
│       └── jobs/                       # RP-C1..RP-C4 + 2 operational prune jobs (6 total)
│           ├── context.go              # jobs.Context dependency bag, jobs.Result, jobs.Func signature
│           ├── trial_expiry.go         # RP-C1
│           ├── trial_hard_delete.go    # RP-C2
│           ├── subscription_lapse.go   # RP-C3 (suspend + offboard transitions)
│           ├── realm_config_reconcile.go # RP-C4
│           ├── outbox_prune.go         # operational — outbox_events retention
│           ├── processed_events_prune.go # operational — processed_events retention
│           └── tx_helper.go            # runInTxWithSysPool for BYPASSRLS raw-SQL sweeps
├── internal/
│   ├── core/
│   │   ├── domain/                     # entities, value objects, DomainError; NO external deps beyond google/uuid
│   │   │   ├── enums.go                # RealmStrategy, RealmStatus, IdpProtocol, ValidationOutcome, SuspensionSource, ReactivationSource, ProcessedEventsConsumer, OpStatus
│   │   │   ├── tenant_realm.go         # TenantRealm entity (tenant_realms row)
│   │   │   ├── idp_config.go           # IdpConfig, IdpConfigValidation
│   │   │   ├── idp_validation.go       # §11.6 CheckOutcome/check-ID constants
│   │   │   ├── provisioning_op.go      # ProvisioningOperation (idempotency ledger row), RealmConfigReconciliation
│   │   │   ├── errors.go               # ErrorCode taxonomy (§17) + Error{Code,Message,Details}, Status() -> HTTP code
│   │   │   └── event.go                # Source="iam-realm-provisioner", SystemActorID, the 9 produced + 5 consumed event-type constants, Event{} struct, every payload struct (§23)
│   │   ├── port/                       # interfaces required by the core
│   │   │   ├── keycloak_admin.go       # KeycloakAdmin — RP-INV-1's sole seam, ~20 methods
│   │   │   ├── repositories.go         # TenantRealmRepository, IdpConfigRepository, IdpValidationRepository, ReconciliationRepository, IdempotencyStore, ProcessedEventsStore
│   │   │   ├── subscription_status_reader.go # SubscriptionStatusReader — OPEN ITEM, see below
│   │   │   ├── openbao_client.go       # OpenBaoClient — IdP secret KV read/write/delete
│   │   │   ├── org_membership_client.go # OrgMembershipClient — I-8 residual-MFA-requirement read
│   │   │   ├── object_store.go         # ObjectStore — PutRealmExport (S3)
│   │   │   ├── event_publisher.go / event_publisher_context.go # EventPublisher, EnqueueCtx, context carrier
│   │   │   ├── tx_runner.go            # TxRunner.RunInTx
│   │   │   └── logger.go               # Logger, SlogStyleLogger, NewSlogStyleLogger
│   │   └── service/                    # use cases — domain+port+requestctx+observability only
│   │       ├── provisioning_service.go # RP-1/2/3/4 (ProvisionTrial, ProvisionDirect, Convert, CreateInvitedUser, RevertConversion)
│   │       ├── lifecycle_service.go    # RP-10/11/12/14/15/16 (Reactivate, GetRealm, ExpireTrial, Suspend, DeleteUser, RevokeSessions)
│   │       ├── idp_service.go          # RP-6/7/13 (ConfigureIdp, RemoveIdp, PortabilityAudit)
│   │       ├── idp_validator.go        # §11.6 Region-Agnostic IdP Configuration Checklist (ValidateIdpConfig)
│   │       ├── realm_config_service.go # RP-8 (ApplyRealmConfig, RP-INV-6 local-intent-first)
│   │       └── mfa_service.go          # RP-9 (ResetMFA) + membership-realm-q's SyncRequiresMfa
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                   # Gin handlers — router.go is the single source of truth for the route table
│       │   │   ├── router.go           # NewRouter: infra routes, docs routes, /api/v1/internal/* (RP-1..RP-16)
│       │   │   ├── handlers.go / lifecycle_handler.go / idp_handler.go / internal_handler.go
│       │   │   ├── middleware.go       # GUCBridgeMiddleware, RequireIdempotencyKey, RequireSystemRole, RequireOperatorRole, RequireJSONContentType
│       │   │   ├── errors.go           # domain.Error -> gincommon.ErrorResponse mapping
│       │   │   └── asyncapi.go         # serves the embedded api/asyncapi.yaml at /asyncapi, /asyncapi.yaml
│       │   └── consumer/               # 3 SQS handlers: membership-realm-q, tenant-realm-q, billing-realm-q
│       └── outbound/
│           ├── postgres/               # repository impls + migrations/000000_initial_schema.{up,down}.sql
│           ├── keycloak/               # gocloak-backed KeycloakAdmin impl — RP-INV-1's ONLY implementation
│           ├── eventbus/               # outbox publisher, Glue/Noop/Validating codecs
│           ├── openbao/                # OpenBao KV v2 + Kubernetes-auth client
│           ├── orgmembership/          # HTTP client for O&M's I-8 residual-MFA check
│           ├── s3/                     # realm-export object store
│           └── metrics/                # cross-cutting Prometheus counters/histograms/gauges (iam_realm_provisioner_*) — the "observability" component
├── internal/eventschema/               # embedded JSON Schema draft-07 files, one per produced event; ByEventType map; asyncapi_drift_test.go cross-checks against api/asyncapi.yaml
├── api/
│   ├── asyncapi.yaml                   # design-time source of truth for the event contract (9 produced + 5 consumed)
│   └── embed.go                        # //go:embed asyncapi.yaml
├── pkg/requestctx/                     # typed RequestContext helpers
├── deploy/
│   ├── helm/iam-realm-provisioner/     # Helm chart — Deployment (server) + 6 CronJob templates (reconciler --job=<name>)
│   ├── keycloak/realm-template.json    # versioned dedicated-realm template (RP-3/RP-2 CreateRealmFromTemplate)
│   ├── iam/                            # AWS IAM policy (SNS/SQS/Glue/S3 — NOT secrets)
│   ├── openbao/                        # OpenBao ACL policy + Kubernetes-auth role, scoped to the IdP secret path
│   └── monitoring/                     # PrometheusRule alerts, SLO burn-rate rules
├── test/{unit,postgres,integration,e2e,fixtures,load}/
├── docs/lld/                           # frozen LLD + IMPLEMENTATION_REFERENCE.md (condensed reference)
├── Dockerfile  docker-compose.yml  Makefile  go.mod  .golangci.yml  .go-arch-lint.yml
```

**`internal/eventschema/` is the one schema-governance workspace** for this repo (no separate design-time-vs-runtime split) — `internal/eventschema/asyncapi_drift_test.go` is this service's CI-enforceable substitute for a live AWS Glue Schema Registry comparison: it asserts `api/asyncapi.yaml`'s `iamTenantEvents` channel message list and `eventschema.ByEventType`'s map keys name exactly the same 9 events.

## Shared Library Dependencies

```go
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon  v1.3.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events     v1.4.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon   v1.2.1
    github.com/Nerzal/gocloak/v13                         v13.9.0
)
```

Module: `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner`, Go 1.26.6.

**Not a dependency of any other IAM service:** `Nerzal/gocloak` — RP-INV-1 requires this to be the *only* package in the entire platform holding a Keycloak admin credential. `.github/scripts/arch-lint.sh` greps every `*.go` file outside `internal/adapter/outbound/keycloak/` for a `Nerzal/gocloak` import and fails CI if found.

### `platform-gincommon` integration

`gincommon.ProtectedMiddlewares` + `ObservabilityMiddlewares` + `TimeoutMiddleware(30s)`. `ServiceName = "iam-realm-provisioner"`. No gRPC — Gin HTTP only, mesh-internal (`/api/v1/internal/*`, no public route). `gincommon.HealthHandler`-style `/healthz` (pure liveness) + hand-rolled `/readyz` (checks Postgres, sys-Postgres, outbox, Keycloak `Ready()`). `/metrics` served on its own dedicated port (`METRICS_PORT=9090`) — never shares a listener with the mesh-internal provisioning API.

### `platform-pgcommon` integration

- `pgcommon.NewPool` with `GUCProvider: pgcommon.GUCSetFromContext` — binds `app.tenant_id` **transaction-locally** (`SET LOCAL` semantics via `set_config(..., is_local => true)`) on every checkout. Never a session-scoped `SET` (RLS-6 — would leak across tenants on a pooled backend under `PG_BOUNCER_MODE=true`).
- A second pool (`SysPool`/`SYSTEM_DATABASE_URL`) uses the `realm_provisioner_migrator` role (`BYPASSRLS`) for the reconciler's cross-tenant sweeps — never the app role.
- `outbox.ApplySchema` runs **before** the domain migration (`000000_initial_schema.up.sql`) — the domain migration ALTERs `outbox_events.payload` from jsonb to text (pgx SimpleProtocol/PgBouncer bytea-hex encoding workaround) and adds a normalize trigger, so `outbox_events` must already exist.
- `TxRunner.RunInTx` wraps every write; the Keycloak mutation for a given endpoint always happens **before** entering `RunInTx` (§9.1 ordering discipline, see `request-flows.md`).

### `platform-events` integration

Single-topic publisher (not a `RoutingPublisher` like some siblings) — one SNS topic (`iam-tenant-events`, `SNS_TOPIC_TENANT_ARN`) carries all 9 produced events. Three separate inbound SQS queues (`membership-realm-q`, `tenant-realm-q`, `billing-realm-q`), each with its own consumer in `internal/adapter/inbound/consumer/`, each with a `-dlq` sibling (`maxReceiveCount=5`). `GLUE_REGISTRY_TENANT_NAME` set → `ValidatingCodec` wraps the Glue wire-format codec (18-byte header + JSON); unset → `NoopCodec` (plain JSON, local dev).

## Dependency Rules (`.go-arch-lint.yml`, enforced in CI via `go-arch-lint` + custom greps)

- **`domain`** — `anyVendorDeps: true` (covers `google/uuid`), no internal deps.
- **`port`** — may depend on `domain` only.
- **`service`** — may depend on `domain`, `port`, `requestctx`, and **`observability`** (`internal/adapter/outbound/metrics`) — `mfa_service.go`, `idp_service.go`, `realm_config_service.go` import it directly to record business-outcome counters at the point of the outcome, matching the sibling convention of treating the metrics package as a cross-cutting instrumentation leaf rather than a "real" adapter.
- **`requestctx`**, **`eventschema`**, **`apispec`** (the `api/` package) — `anyVendorDeps: true`, standalone leaves.
- **`observability`** — may depend on `domain` only.
- **`keycloak`** (`internal/adapter/outbound/keycloak`) — its own component, separate from `adapters_outbound`, specifically so the RP-INV-1 single-writer invariant can be verified structurally: no other `adapters_outbound` package needs to depend on it, and `.github/scripts/arch-lint.sh` additionally greps for stray `gocloak` imports outside this one package.
- **`adapters_outbound`** (`postgres`, `eventbus`, `openbao`, `orgmembership`, `s3`) — may depend on `domain`, `port`, `service`, `eventschema`, `observability`.
- **`adapters_inbound`** (`http`, `consumer`) — may depend on `domain`, `port`, `service`, `requestctx`, `apispec`, `observability`.
- **`reconciler_jobs`** (`cmd/reconciler/jobs`) — its own component (not merged into `cmd`) so it can be declared as a normal dependency of `cmd/reconciler`'s `main.go` without a false-positive self-cycle warning. May depend on `domain`, `port`, `observability`.
- **`cmd`** (`cmd/server`, `cmd/reconciler`) — the composition root; may import everything, including `keycloak`, both adapter groups, and `reconciler_jobs`.

Enforced via `deepScan: false` (import-level checks only — a composition root wiring an adapter into a service constructor is how Clean Architecture is supposed to work in `main.go`, not a violation) and `exclude: [test, scripts, deploy]` / `excludeFiles: ['.*_test\.go$']`.

**Two additional CI-enforced invariants beyond the layering check** (`.github/scripts/arch-lint.sh`):
- **RP-INV-1** — `grep -rl "Nerzal/gocloak" --include="*.go" .` outside `internal/adapter/outbound/keycloak/` must be empty.
- **RLS-6** — `grep -rn "SET app\.tenant_id"` across `*.go`/`*.sql` must only ever appear as `SET LOCAL`.
