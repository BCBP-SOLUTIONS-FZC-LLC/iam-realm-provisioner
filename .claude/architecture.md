# Architecture

Narrative counterpart: [`ARCHITECTURE.md`](../ARCHITECTURE.md) (layer model, composition root, ports, degradation matrix, invariants — structured to match `iam-org-membership/ARCHITECTURE.md`).

Refines LLD §3, §6, §6.2. Clean Architecture / Ports-and-Adapters — dependencies point inward; `core/domain` imports nothing internal. `cmd/server/main.go` (HTTP API + 3 SQS consumers + outbox runner) and `cmd/reconciler/main.go` (CronJob dispatcher, `--job=<name>`) are the two composition roots; both ship from one Docker image (see Dockerfile).

## Package Layout

```
iam-realm-provisioner/
├── cmd/
│   ├── server/
│   │   ├── main.go                     # HTTP composition root: pool+GUC wiring, MigrateSchema unless RUN_MIGRATIONS=false, outbox runner (eventcfg), SQS consumers (sqspipeline + DLQ publisher), graceful shutdown
│   │   ├── identity.go                 # resolveAppEnv / isDevEnv / loggerEnv — one environment for metrics, traces and logs
│   │   ├── wiring.go                   # DI helpers factored out of main.go (Glue codec, SNS publisher, runMigrationsAtStartup)
│   │   ├── inbound_schema.go           # consumedValidator — eventschema.ConsumedByConsumer → sqspipeline.Validator
│   │   ├── audit_delivery.go           # AL-5 pending_audit_entries drainer goroutine
│   │   └── exporters.go                # runBusinessExporters — goroutines refreshing the §11.2 DB-state gauges + rls_violations_total
│   ├── migrate/
│   │   └── main.go                     # migrate hook Job (Helm pre-install/pre-upgrade): postgres.MigrateSchema over MIGRATION_DATABASE_URL
│   └── reconciler/
│       ├── main.go                     # CronJob dispatcher, --job=<name>, shared jctx.Context construction, Pushgateway push
│       ├── identity.go                 # resolveAppEnv / isDevEnv / loggerEnv (same as cmd/server)
│       ├── noop_publisher.go           # no-op EventPublisher for a dry-run/local invocation
│       └── jobs/                       # RP-C1..RP-C4 + 3 operational prune jobs (7 total)
│           ├── context.go              # jobs.Context dependency bag, jobs.Result, jobs.Func signature
│           ├── trial_expiry.go         # RP-C1
│           ├── trial_hard_delete.go    # RP-C2 — uses NewGDPRScrubFn to atomically delete keycloak_idp_configs, keycloak_idp_config_validations, realm_config_reconciliations, provisioning_operations, pending_audit_entries
│           ├── subscription_lapse.go   # RP-C3 (suspend + offboard transitions) — offboard phase uses NewGDPRScrubFn for the same five-table PII erasure
│           ├── realm_config_reconcile.go # RP-C4
│           ├── outbox_prune.go         # operational — outbox_events retention
│           ├── processed_events_prune.go # operational — processed_events retention
│           ├── ledger_prune.go         # operational — provisioning_operations + rls_violation_log retention
│           ├── gdpr.go                 # — GDPR PII scrub shared by RP-C2 and RP-C3 (NewGDPRScrubFn)
│           └── tx_helper.go            # runInTxWithSysPool for BYPASSRLS raw-SQL sweeps
├── internal/
│   ├── core/
│   │   ├── domain/                     # entities, value objects, DomainError; NO external deps beyond google/uuid
│   │   │   ├── enums.go                # RealmStrategy, RealmStatus, IdpProtocol, ValidationOutcome, SuspensionSource, ReactivationSource, ProcessedEventsConsumer, OpStatus
│   │   │   ├── tenant_realm.go         # TenantRealm entity (tenant_realms row)
│   │   │   ├── idp_config.go           # IdpConfig, IdpConfigValidation
│   │   │   ├── idp_validation.go       # §11.6 CheckOutcome/check-ID constants
│   │   │   ├── provisioning_op.go      # ProvisioningOperation (idempotency ledger row), RealmConfigReconciliation
│   │   │   ├── audit_entry.go          # AuditEntry / PendingAuditEntry — the AL-5 TenantIdpConfigChanged direct-write entry
│   │   │   ├── errors.go               # ErrorCode taxonomy (§17) + Error{Code,Message,Details}, Status() -> HTTP code
│   │   │   └── event.go                # Source="iam-realm-provisioner", SystemActorID, the 9 produced + 6 consumed event-type constants, Event{} struct, every payload struct (§25)
│   │   ├── port/                       # interfaces required by the core
│   │   │   ├── keycloak_admin.go       # KeycloakAdmin — RP-INV-1's sole seam, 29 methods, incl. service-account
│   │   │   │                          #   client mint (CreateServiceAccountClient -> sub, created bool, err —
│   │   │   │                          #   created reports fresh-mint vs resumed, §2.5), ClearServiceAccountKeysCache,
│   │   │   │                          #   DeleteServiceAccountClientIfExists (idempotent, mirrors DeleteRealmIfExists)
│   │   │   ├── repositories.go         # TenantRealmRepository, IdpConfigRepository, IdpValidationRepository, ReconciliationRepository, IdempotencyStore, Inbox, InboxPruner, PendingAuditEntryRepository, RetentionPruner
│   │   │   ├── audit_log_client.go     # AuditLogClient — AL-5 direct-write ingest
│   │   │   ├── token_service_client.go # TokenServiceClient — TS-1/TS-3/TS-4 (§2.5)
│   │   │   ├── subscription_status_reader.go # SubscriptionLapseReader — RP-C3 suspend-phase trigger, backed by O&M's I-16
│   │   │   ├── openbao_client.go       # OpenBaoClient — IdP secret KV read/write/delete
│   │   │   ├── org_membership_client.go # OrgMembershipClient — I-8 residual-MFA-requirement read
│   │   │   ├── object_store.go         # ObjectStore — PutRealmExport (S3)
│   │   │   ├── event_publisher.go                 # EventPublisher.Enqueue(ctx, event); tx lives on ctx
│   │   │   ├── tx_runner.go            # TxRunner.RunInTx
│   │   │   └── logger.go               # Logger, SlogStyleLogger, NewSlogStyleLogger
│   │   └── service/                    # use cases — domain+port+requestctx+observability only
│   │       ├── provisioning_service.go # RP-1/2/3/4/17 (ProvisionTrial, ProvisionDirect, Convert, CreateInvitedUser,
│   │       │                          #   RP-3 runs detached from the request (CONVERT_TIMEOUT), Drain waits for it at shutdown;
│   │       │                          #   RevertConversion, RefreshServiceAccountKeys); mintPlatformAutomation is the
│   │       │                          #   shared RP-1/2/3/4 mint helper (§2.5) — Convert also retires the tenant's old
│   │       │                          #   shared-realm client on success; RevertConversion re-mints one back into the
│   │       │                          #   shared realm since DeleteRealmIfExists just took the dedicated one down
│   │       ├── lifecycle_service.go    # RP-10/11/12/14/15/16 (Reactivate, GetRealm, ExpireTrial, Suspend, DeleteUser, RevokeSessions)
│   │       ├── idp_service.go          # RP-6/7/13 (ConfigureIdp, RemoveIdp, PortabilityAudit)
│   │       ├── idp_validator.go        # §11.6 Region-Agnostic IdP Configuration Checklist (ValidateIdpConfig)
│   │       ├── realm_config_service.go # RP-8 (ApplyRealmConfig, RP-INV-6 local-intent-first)
│   │       ├── mfa_service.go          # RP-9 (ResetMFA) + membership-realm-q's SyncRequiresMfa
│   │       ├── audit_delivery_service.go # AL-5 drain: claim due pending_audit_entries, deliver, delete / back off / park failed
│   │       └── compensation.go         # compensationContext — detached 30s context for compensating teardown
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                   # Gin handlers — router.go is the single source of truth for the route table
│       │   │   ├── router.go           # NewRouter: infra routes, docs routes, /api/v1/internal/* (RP-1..RP-17)
│       │   │   ├── handlers.go / lifecycle_handler.go / idp_handler.go / internal_handler.go
│       │   │   ├── middleware.go       # GUCBridgeMiddleware, RequireIdempotencyKey, RequireSystemRole, RequireOperatorRole, RequireTenantMatch, RequireJSONContentType
│       │   │   ├── errors.go           # domain.Error -> gincommon.ErrorResponse mapping
│       │   │   ├── asyncapi.go         # interactive dashboard + raw-YAML serving for the embedded api/asyncapi.yaml, at /asyncapi, /asyncapi.yaml
│       │   │   └── swagger_theme.go / swagger_initializer.go  # custom-themed assets for GET /swagger/*any (swag-generated OpenAPI 2.0 UI, all 17 RP-1..RP-17 handlers)
│       │   ├── consumer/               # 3 SQS handlers: membership-realm-q, tenant-realm-q, billing-realm-q (dedup via port.Inbox)
│       │   └── sqspipeline/            # builds the consumers from platform-events: EnvForQueue (eventcfg.LoadSQS), NewConsumer (GlueDecodeCodec + WithDLQForwarding), RequireDLQ, Handler (DLQ route → span → schema validation)
│       └── outbound/
│           ├── postgres/               # repository impls + migrations/000000_initial_schema.{up,down}.sql + gauge_repository.go (§11.2 exporters) + inbox_repository.go (platform-events inbox.Store) + migrate.go (MigrateSchema) + idempotency_repository.go (lease reclaim, detached Complete/Fail) + retention_repository.go (ledger-prune) + pending_audit_entry_repository.go (AL-5 queue)
│           ├── keycloak/               # gocloak-backed KeycloakAdmin impl — RP-INV-1's ONLY implementation + instrumentation.go (OTel spans on gocloak's resty transport)
│           ├── eventbus/               # outbox publisher, Glue/Noop/Validating codecs
│           ├── openbao/                # OpenBao KV v2 + Kubernetes-auth client
│           ├── orgmembership/          # HTTP client for O&M's I-8 residual-MFA check + I-16 subscription-lapse poll (RP-C3, port.SubscriptionLapseReader)
│           ├── auditlog/               # HTTP client for Audit Log's AL-5 direct-write ingest (port.AuditLogClient)
│           ├── tokenservice/           # HTTP client for Token Service's TS-1/TS-3/TS-4 credential-lifecycle API (§2.5, port.TokenServiceClient)
│           ├── s3/                     # realm-export object store
│           ├── httpx/                  # cross-cutting leaf: the one HTTP transport every outbound adapter builds on, injecting W3C traceparent (§11.3)
│           └── metrics/                # cross-cutting Prometheus counters/histograms/gauges (iam_realm_provisioner_*) — the "observability" component
├── internal/eventschema/               # embedded JSON Schema draft-07: 9 produced (ByEventType, Glue-registered) + 6 consumed (ConsumedByConsumer); asyncapi_drift_test.go cross-checks against api/asyncapi.yaml
├── api/
│   ├── asyncapi.yaml                   # design-time source of truth for the event contract (9 produced + 6 consumed)
│   └── embed.go                        # //go:embed asyncapi.yaml
├── cmd/server/swagger_info.go           # swag global API metadata (@title/@BasePath/@securityDefinitions/@tag) — `make swag`'s -g entry point
├── docs/swagger/                        # generated by `make swag` (docs.go, swagger.json, swagger.yaml) — checked in, blank-imported by cmd/server/main.go to self-register at init
├── pkg/requestctx/                     # typed RequestContext helpers
├── deploy/
│   ├── helm/                           # Helm chart — Deployment (server), migrate hook Job (+ hook NetworkPolicy), 7 CronJobs (reconciler --job=<name>)
│   ├── keycloak/realm-template.json    # versioned dedicated-realm template (RP-3/RP-2 CreateRealmFromTemplate)
│   ├── iam/                            # AWS IAM policy (SNS/SQS/Glue/S3 — NOT secrets)
│   ├── openbao/                        # OpenBao ACL policy + Kubernetes-auth role, scoped to the IdP secret path
│   └── monitoring/                     # PrometheusRule alerts (app-alerts.yml, schema-registry-alerts.yml), SLO burn-rate rules, prometheus-adapter rule
├── .github/
│   ├── workflows/                      # ci.yml, validate-test.yml, validate-quality.yml, changelog-check.yml,
│   │                                    # release.yml, freeze-watchdog.yml, schema-registry.yml, schema-prune.yml, schema-health-quarterly.yml (9 total)
│   └── scripts/                        # arch-lint.sh, coverage-gate.sh, check-metric-naming.sh, metricslint.sh, metrics-inventory.py,
│                                        # metrics-registry-lint.sh, smoke-tests.sh, release/schema-gov helper scripts
├── .githooks/pre-commit                # tidy + fmt-check + vet + lint; installed via `make setup`/`make install-hooks`
├── test/{unit,postgres,integration,e2e,load,dbseed}/
├── docs/lld/                           # LLD v1.23 (names frozen at v1.0)
├── docs/observability/                 # Observability Standard README, generated metric-registry.md, migration.md, runbooks.md (one entry per alert)
├── docs/runbook.md                     # operator procedures for the headline incident scenarios (§24)
├── docs/runbook-schema-registry.md     # schema-governance operator runbook (shared iam-tenant-events registry)
├── VERSIONING.md                       # SemVer policy + maintainer release process (release.yml)
├── Dockerfile  docker-compose.yml  Makefile  go.mod  .golangci.yml  .go-arch-lint.yml
    # docker-compose.yml's `floci`/`floci-ui` services replace LocalStack —
    # Floci's free tier covers SNS/SQS/S3/Glue, so the former Pro-tier
    # split (docker-compose.pro.yml, retired) is no longer needed
```

**`internal/eventschema/` is the local half of schema governance** — `internal/eventschema/asyncapi_drift_test.go` is a fast, no-AWS CI check: it asserts `api/asyncapi.yaml`'s `iamTenantEvents` channel message list and `eventschema.ByEventType`'s map keys name exactly the same 9 events. (The 6 consumed schemas beside them are extracted too — `schema-gov extract --check` must report all 15 in sync — but only enforced at consume time, never registered.) The live-registry half is `platform-schemagov` (`schema-gov` CLI, Docker image, zero Go-code presence): `.github/workflows/schema-registry.yml` validates/diffs/registers this service's 9 schemas against the shared `iam-tenant-events` Glue registry (O&M's `TenantCreated`/`TrialStarted` share the registry but are never touched — no exclusion flag exists upstream, so `schema-prune.yml` hard-blocks `--execute` while either name is present), `schema-prune.yml` and `schema-health-quarterly.yml` cover pruning and quarterly lint. `make schema-*` runs the same checks locally. See `docs/runbook-schema-registry.md`. `api/asyncapi.yaml` now declares 7 servers: the SNS topic + 3 inbound SQS queues + 3 downstream Commercial consumer queue references (`sqsTenantBilling`, `sqsTenantEntitlements`, `sqsTenantUsage`) added for cross-team visibility (CD-26). The 4 channels are unchanged — the downstream queues are documentation only in the server block.

**DB-state exporters** (`cmd/server/exporters.go`, `runBusinessExporters`) poll `pgadapter.GaugeRepository` on the BYPASSRLS `sysPool` every 5 minutes: three gauges (`realm_active_gauge`, `realm_config_pending_gauge`, `template_drift`) plus one monotonic counter (`rls_violations_total`, `Add`ed not `Set`) scraped from `rls_violation_log` — backing the two RLS-6 alerts (`IAMRealmProvisionerRLSCrossTenantAccess` critical, `RLSMissingGUC` warning). The counter's exporter needs a Terraform-side `GRANT SELECT ON rls_violation_log TO realm_provisioner_migrator` this repo cannot itself provide — see `.claude/operations.md` §20.

**Tracing.** `internal/adapter/outbound/httpx` is the one HTTP transport every outbound adapter builds on (O&M, OpenBao, and — via `RestyClient().SetTransport` — every Keycloak Admin API call through `internal/adapter/outbound/keycloak/instrumentation.go`), injecting W3C `traceparent` from the caller's context so a provision → event → O&M projection stays one trace. `pgcommon.Config.Tracer` is `gincommon.NewSpanTracer(serviceName)` (a `db.query` span per query). Both composition roots bootstrap the tracer provider via `gincommon.InitTracingWithConfig()` with the same identity as the metrics, unconditionally, so in-process spans get valid trace IDs even without a collector.

## Shared Library Dependencies

```go
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon  v1.6.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2     v2.1.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2   v2.0.1
    github.com/Nerzal/gocloak/v13                         v13.9.0
)
```

Module: `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner`, Go 1.26.9.

**Not a dependency of any other IAM service:** `Nerzal/gocloak` — RP-INV-1 requires this to be the *only* package in the entire platform holding a Keycloak admin credential. `.github/scripts/arch-lint.sh` greps every `*.go` file outside `internal/adapter/outbound/keycloak/` for a `Nerzal/gocloak` import and fails CI if found.

### `platform-gincommon` integration

`gincommon.ProtectedMiddlewares` + `ObservabilityMiddlewares` with `Config.RequestTimeout = 30s` (gincommon appends `TimeoutMiddleware` innermost, so metrics/logs/spans record the client's 503). `/metrics` is `gincommon.MetricsHandler()`. `ServiceName = "iam-realm-provisioner"`. No gRPC — Gin HTTP only, mesh-internal (`/api/v1/internal/*`, no public route). `gincommon.HealthHandler`-style `/healthz` (pure liveness) + hand-rolled `/readyz` (checks Postgres, sys-Postgres, outbox, Keycloak `Ready()`). `/metrics` served on its own dedicated port (`METRICS_PORT=9090`) — never shares a listener with the mesh-internal provisioning API.

### `platform-pgcommon` integration

- Both pools start from `pgcommon.ConfigFromEnv()` (DSN assembly, sizing, `PG_BOUNCER_MODE`, slow-query threshold — one implementation, the library's); `main.go` overrides only `DSN` (`pgadapter.DSNFromEnv`, which adds `PG_STATEMENT_TIMEOUT`), `GUCProvider`, `Logger`, and `Tracer`. Nothing in the repo calls `pgxpool.New`; test superuser handles go through `test/dbseed` (`pgcommon.NewPool`).
- `pgcommon.NewPool` with `GUCProvider: pgcommon.GUCSetFromContext` — binds `app.tenant_id` **transaction-locally** (`SET LOCAL` semantics via `set_config(..., is_local => true)`) on every checkout. Never a session-scoped `SET` (RLS-6 — would leak across tenants on a pooled backend under `PG_BOUNCER_MODE=true`).
- A second pool (`SysPool`/`SYSTEM_DATABASE_URL`) uses the `realm_provisioner_migrator` role (`BYPASSRLS`) for the reconciler's cross-tenant sweeps — never the app role. Build it with `pgadapter.SystemPoolConfig(dsn, log)` (same helper as `iam-org-membership`), **never** a bare `pgcommon.Config{DSN: …}` literal: a literal drops `PGBouncerMode` and the pool then breaks behind PgBouncer with `prepared statement already exists`. The helper forces `PGBouncerMode: true`, clears `GUCProvider` (BYPASSRLS must see every tenant), inherits ConfigFromEnv pool sizing, and wires the gincommon logger.
- `/readyz` probes both pools with `Pool.Health(ctx)` (pings **and** reads pool stats), not bare `Ping`.
- `outbox.ApplySchema` runs **before** the domain migration (`000000_initial_schema.up.sql`) — the domain migration ALTERs `outbox_events.payload` from jsonb to text (pgx SimpleProtocol/PgBouncer bytea-hex encoding workaround) and adds a normalize trigger, so `outbox_events` must already exist.
- `TxRunner.RunInTx` wraps every write; the Keycloak mutation for a given endpoint always happens **before** entering `RunInTx` (§9.1 ordering discipline, see `request-flows.md`).

### `platform-events` integration

Single-topic publisher (not a `RoutingPublisher` like some siblings) — one SNS topic (`iam-tenant-events`, `SNS_TOPIC_TENANT_ARN`) carries all 9 produced events. Three separate inbound SQS queues (`membership-realm-q`, `tenant-realm-q`, `billing-realm-q`), each with its own consumer in `internal/adapter/inbound/consumer/`, each with a `-dlq` sibling (`maxReceiveCount=5`). `GLUE_REGISTRY_TENANT_NAME` set → `ValidatingCodec` wraps the Glue wire-format codec (18-byte header + JSON); unset → `NoopCodec` (plain JSON, local dev).

## Dependency Rules (`.go-arch-lint.yml`, enforced in CI via `go-arch-lint` + custom greps)

- **`domain`** — `anyVendorDeps: true` (covers `google/uuid`), no internal deps.
- **`port`** — may depend on `domain` only.
- **`service`** — may depend on `domain`, `port`, `requestctx`, and **`observability`** (`internal/adapter/outbound/metrics`) — `mfa_service.go`, `idp_service.go`, `realm_config_service.go` import it directly to record business-outcome counters at the point of the outcome, matching the sibling convention of treating the metrics package as a cross-cutting instrumentation leaf rather than a "real" adapter.
- **`requestctx`**, **`eventschema`**, **`apispec`** (the `api/` package) — `anyVendorDeps: true`, standalone leaves.
- **`observability`** (`internal/adapter/outbound/metrics`) — `anyVendorDeps: true` with no internal deps at all — it imports nothing from `domain`/`port`, matching `httptransport`'s shape exactly.
- **`httptransport`** (`internal/adapter/outbound/httpx`) — its own component, same rationale as `observability`: a cross-cutting leaf every outbound HTTP caller depends on (W3C traceparent injection, §11.3, plus the shared `CloseBody` helper every outbound client's `defer` calls), not a "real" adapter. `anyVendorDeps: true` with no internal deps at all — not even `domain`.
- **`keycloak`** (`internal/adapter/outbound/keycloak`) — its own component, separate from `adapters_outbound`, specifically so the RP-INV-1 single-writer invariant can be verified structurally: no other `adapters_outbound` package needs to depend on it, and `.github/scripts/arch-lint.sh` additionally greps for stray `gocloak` imports outside this one package. May depend on `domain`, `port`, `observability`, `httptransport`.
- **`postgres`** (`internal/adapter/outbound/postgres`) — its own component too, separate from `adapters_outbound`, so `eventbus` can import its `TxFromContext` without a false intra-component cycle (tx lives on ctx, matching `iam-org-membership`'s `EventPublisher`). Narrower `mayDependOn` than the rest of outbound: `domain`, `port` only — no `service`/`eventschema`/`observability`/`httptransport`.
- **`adapters_outbound`** (`eventbus`, `openbao`, `orgmembership`, `s3`, `tokenservice` — **not** `postgres` or `keycloak`, both their own components above) — may depend on `domain`, `port`, `service`, `eventschema`, `observability`, `httptransport`, `postgres`.
- **`adapters_inbound`** (`http`, `consumer`) — may depend on `domain`, `port`, `service`, `requestctx`, `apispec`, `observability`.
- **`reconciler_jobs`** (`cmd/reconciler/jobs`) — its own component (not merged into `cmd`) so it can be declared as a normal dependency of `cmd/reconciler`'s `main.go` without a false-positive self-cycle warning. May depend on `domain`, `port`, `observability`.
- **`cmd`** (`cmd/server`, `cmd/reconciler`) — the composition root; may import everything, including `keycloak`, both adapter groups, and `reconciler_jobs`.

Enforced via `deepScan: false` (import-level checks only — a composition root wiring an adapter into a service constructor is how Clean Architecture is supposed to work in `main.go`, not a violation) and `exclude: [test, scripts, deploy]` / `excludeFiles: ['.*_test\.go$']`.

**Two additional CI-enforced invariants beyond the layering check** (`.github/scripts/arch-lint.sh`):
- **RP-INV-1** — `grep -rl "Nerzal/gocloak" --include="*.go" .` outside `internal/adapter/outbound/keycloak/` must be empty.
- **RLS-6** — `grep -rn "SET app\.tenant_id"` across `*.go`/`*.sql` must only ever appear as `SET LOCAL`.
