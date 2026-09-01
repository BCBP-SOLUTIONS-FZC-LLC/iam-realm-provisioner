# Architecture

This document describes the internal structure, dependency rules, and runtime invariants of `iam-realm-provisioner`.

`iam-realm-provisioner` is a private Go service (`github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner`, Go 1.26.6) that ships as two binaries built from one image: `cmd/server` (the HTTP API, RP-1..RP-16, plus the three inbound SQS consumers and the transactional outbox runner) and `cmd/reconciler` (a single binary dispatched by `--job=<name>`, covering RP-C1..RP-C4 and two operational prune jobs, each its own Kubernetes CronJob). It refines the IAM HLD; LLD v1.0 Approved (`docs/lld/iam-lld-realm-provisioner-service.md`), condensed for implementation at `docs/lld/IMPLEMENTATION_REFERENCE.md`.

The service is the **single writer to the Keycloak Admin API** across the entire IAM subsystem, and the system of record for the small amount of local state — realm identity, IdP config, reconciliation ledgers — needed to make that writing safe, idempotent, and replayable.

---

## Layer model

Clean Architecture — dependencies point inward; outer layers never import inner layers. Enforced in CI by `go-arch-lint` against `.go-arch-lint.yml`.

```
iam-realm-provisioner/
├── cmd/
│   ├── server/                       # Composition root: pool setup, migrations, adapters, router, SQS consumers, outbox runner
│   └── reconciler/                   # Single binary dispatched by --job=<name>; drives RP-C1..RP-C4 + 2 prune jobs
│       └── jobs/                     # One file per job
├── internal/
│   ├── core/
│   │   ├── domain/                   # Entities, enums, DomainError catalogue, event payload structs (no external deps)
│   │   ├── port/                     # Interfaces: repositories, KeycloakAdmin, IdempotencyStore, ProcessedEventsStore,
│   │   │                             #   SubscriptionStatusReader, Logger
│   │   └── service/                  # Use cases: ProvisioningService, LifecycleService, MfaService, IdpService, RealmConfigService
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                 # Gin handlers, router, middleware, AsyncAPI doc pages
│       │   └── consumer/             # Three SQS consumers (membership/tenant/billing)
│       └── outbound/
│           ├── postgres/             # Repository impls + golang-migrate migrations + TxRunner
│           ├── keycloak/             # RP-INV-1's SOLE seam — the only package permitted to import Nerzal/gocloak
│           ├── openbao/              # KV v2 + Kubernetes-auth client for IdP secrets
│           ├── orgmembership/        # HTTP client — I-8 residual-MFA-requirement read
│           ├── s3/                   # Realm export destination (RP-C3 pre-offboard export)
│           ├── eventbus/             # ValidatingCodec (outbox-enqueue time) + GlueCodec (SNS-publish time) + outbox wiring
│           └── metrics/              # Prometheus counters/histograms (iam_realm_provisioner_*)
├── internal/eventschema/             # Embedded JSON Schema Draft-07 for the 9 published events + drift test
├── api/
│   ├── asyncapi.yaml                 # AsyncAPI 3.0 — design-time source of truth for the event contract
│   └── embed.go                      # //go:embed asyncapi.yaml
├── docs/lld/                         # Frozen LLD + condensed IMPLEMENTATION_REFERENCE.md
├── deploy/                           # Helm chart, IAM policy, OpenBao policy, monitoring alerts
├── scripts/                          # Local dev tooling (init-db.sql, init-localstack.sh, keycloak-bootstrap.sh, merge_coverage.py)
└── test/
    ├── unit/                         # Fast unit tests — no Docker required
    ├── postgres/                     # RLS + DB integration (testcontainers-go)
    ├── integration/                  # Cross-layer tests (testcontainers Postgres + Keycloak 26)
    └── e2e/                          # End-to-end
```

### Dependency rules (`.go-arch-lint.yml`)

| Component | In | May import |
|---|---|---|
| `domain` | `internal/core/domain` | Nothing internal |
| `port` | `internal/core/port` | `domain` |
| `requestctx` | `pkg/requestctx` | Nothing internal |
| `eventschema` | `internal/eventschema` | Nothing internal (embedded assets) |
| `apispec` | `api` | Nothing internal (embedded `asyncapi.yaml`) |
| `observability` | `internal/adapter/outbound/metrics` | `domain` |
| `service` | `internal/core/service` | `domain`, `port`, `requestctx`, `observability` |
| `keycloak` | `internal/adapter/outbound/keycloak` | `domain`, `port`, `observability` |
| `adapters_outbound` | `postgres`, `eventbus`, `openbao`, `orgmembership`, `s3` | `domain`, `port`, `service`, `eventschema`, `observability` |
| `adapters_inbound` | `http`, `consumer` | `domain`, `port`, `service`, `requestctx`, `apispec`, `observability` |
| `reconciler_jobs` | `cmd/reconciler/jobs` | `domain`, `port`, `observability` |
| `cmd` | `cmd/server`, `cmd/reconciler` | Everything above |

`service` is allowed to import `observability` directly (`mfa_service.go`, `idp_service.go`, `realm_config_service.go` emit business metrics) — a documented cross-cutting exception, matching the sibling IAM services' convention, rather than a general adapter dependency. `keycloak` is its own component, separate from the rest of `adapters_outbound`, specifically so RP-INV-1 can be checked structurally: no other outbound adapter package needs to depend on it, and CI additionally greps every `.go` file outside `internal/adapter/outbound/keycloak/` for a `Nerzal/gocloak` import.

`test/`, `scripts/`, and `deploy/` are excluded from lint scope — they aren't part of the runtime architecture.

---

## RP-INV-1 — single Keycloak writer

Every mutating and read operation this service performs against Keycloak's Admin API is expressed as one method on `port.KeycloakAdmin` (`internal/core/port/keycloak_admin.go`) — `CreateRealmFromTemplate`, `RegisterOIDCClients`, `MigrateUser`, `CreateOrUpdateIdp`, `AssignRequiresMFA`, `RevokeUserSessions`, etc. Exactly one adapter implements it: `internal/adapter/outbound/keycloak/` (package `keycloak`), backed by `Nerzal/gocloak`. Every core service (`ProvisioningService`, `LifecycleService`, `MfaService`, `IdpService`, `RealmConfigService`) depends only on the `port.KeycloakAdmin` interface, never on gocloak or an HTTP client of its own.

Two layers of enforcement:
1. **Structural** — `.go-arch-lint.yml` puts `keycloak` in its own component; no other component's dependency rule permits it, so nothing outside that one package can even compile against gocloak's types without also depending on the `keycloak` component (which nothing is allowed to declare).
2. **Textual** — `.github/scripts/arch-lint.sh` greps every `.go` file in the repo for `Nerzal/gocloak` and fails the build if a match exists outside `internal/adapter/outbound/keycloak/` (test files excluded, since integration tests legitimately talk to a real Keycloak container directly to verify the adapter's own behavior).

---

## RLS-6 — no session-scoped tenant GUC

Five tables are tenant-scoped and carry `FORCE ROW LEVEL SECURITY`: `tenant_realms`, `keycloak_idp_configs`, `keycloak_idp_config_validations`, `realm_config_reconciliations`, `provisioning_operations`. `outbox_events` (owned by `platform-events`' `outbox.ApplySchema`) and `processed_events` are RLS-exempt by design — the outbox spans tenants by nature, and dedup keys are event IDs, not tenant-scoped rows.

The app pool is constructed with `pgcommon.GUCProvider = pgcommon.GUCSetFromContext`, which binds `app.tenant_id` **transaction-locally** (`SELECT set_config('app.tenant_id', $1, true)`, `is_local=true`) on every checkout. Under PgBouncer transaction-pooling mode (`PG_BOUNCER_MODE=true` in production), a session-scoped `SET app.tenant_id = ...` would persist on a pooled backend and leak the previous tenant's GUC to the next request that reuses the connection — this is exactly what `test/postgres/rls_test.go::TestRLS_Case5_NoGUCLeakageAcrossPooledConnection` verifies (that test pins the pool with `PGBouncerMode: true` specifically to exercise the transaction-scoped code path, since the session-scoped path — used when `PGBouncerMode: false` — does not clear a prior tenant's GUC on a no-GUC checkout, a real behavior difference in `platform-pgcommon` discovered while writing this test).

`.github/scripts/check-forbidden-set-guc.sh` (CI, `validate-quality.yml`) and the equivalent grep inside `.github/scripts/arch-lint.sh` (CI, `validate-test.yml`) both reject any non-`SET LOCAL`, non-`set_config(...)` write to `app.tenant_id` in Go or SQL source under `cmd/`/`internal/`.

The reconciler jobs (`cmd/reconciler`) never use the RLS-scoped app pool for their sweeps — they're constructed against `SysPool`, a second `pgcommon.Pool` built from `SYSTEM_DATABASE_URL` with no `GUCProvider` at all (BYPASSRLS, must target the `realm_provisioner_migrator` role in production). A reconciler job spanning every tenant would otherwise see zero rows under RLS with no GUC bound.

---

## Idempotency and replay safety

Two ledgers, serving different purposes:

- **`provisioning_operations`** — every mutating HTTP endpoint (RP-1..RP-10, RP-12, RP-14..RP-16) requires a caller-supplied `Idempotency-Key` header. The service layer's first line is always `s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.Op...)`, which returns `(existing, found, err)`. If `found` and the stored operation's status is `succeeded`, the handler replays the stored JSON result verbatim instead of re-executing (`replayOrConflict[T]`); if `found` and still `in_progress`, it returns `409 operation_in_progress`. Every exit path calls either `idempotency.Complete(ctx, key, resultJSON)` on success or `idempotency.Fail(ctx, key)` on any error — there is no path that leaves an operation row permanently `in_progress` after a definite outcome.
- **`processed_events`** — the three SQS consumers (membership/tenant/billing) dedup inbound events by `(event_id, consumer)`, independent of the idempotency ledger above (which is keyed by a caller header, not an event ID). RLS-exempt, since consumers process events across every tenant.

---

## Keycloak-effect-first-then-local-commit ordering, and compensating teardown

Every provisioning flow in `ProvisioningService` follows the same discipline: Keycloak mutations happen first, outside any Postgres transaction; only once every Keycloak step has succeeded does the local `tenant_realms` row update and outbox event enqueue happen together inside one `s.tx.RunInTx(...)`. This ordering means a Keycloak failure at any point leaves **no** local trace — no phantom `tenant_realms` row describing a realm that doesn't actually exist — because the local write is always the last step.

`Convert` (RP-3) is the clearest example of the pattern, since it's genuinely multi-step against Keycloak: create the dedicated realm → register OIDC clients → list the shared realm's users for this tenant → migrate each user one at a time (`MigrateUser`, which internally deletes the user from the shared/source realm before recreating it with the same ID in the dedicated realm, since Keycloak's user ID is a global cross-realm primary key) → force-logout the shared realm's remaining sessions (best-effort, logged but non-fatal) → **then** the single `RunInTx` that flips `tenant_realms.strategy` to `dedicated`, records the new realm name/shard, and enqueues both `TenantRealmReady` and `TenantConverted`.

If any Keycloak step from realm creation through the user-migration loop fails, the handler calls `s.keycloak.DeleteRealmIfExists(ctx, realmName)` before returning the error — tearing down the partially-created dedicated realm so a retry (same `Idempotency-Key`, since `Fail()` was called, not `Complete()`) starts clean. The shared/source realm is never touched by this teardown: any users already migrated out of it in the loop stay migrated (`MigrateUser`'s own semantics already deleted them from the source), and a retry simply re-lists whatever remains in the shared realm via a fresh `FindByTenantID` + `ListUsersByTenant` call and continues from there. `ProvisionDirect` and `RevertConversion` apply the identical realm-creation-then-teardown-on-failure pattern for their own Keycloak sequences.

---

## Transactional outbox and migration ordering

Events are published via `platform-events`' transactional outbox: `EnqueueCtx` inserts into `outbox_events` inside the same `RunInTx` as the domain state change, and a separate `outbox.Runner` goroutine polls the table (`OUTBOX_POLL_INTERVAL`, default 500ms) and publishes to SNS. `internal/eventschema`'s embedded JSON Schemas validate every payload's shape at enqueue time via a `ValidatingCodec`; a separate `GlueCodec` prepends the AWS Glue wire-format header (18 bytes: 1 version byte, 1 compression byte, 16-byte schema-version UUID) only at the SNS-publish boundary — the `outbox_events` row itself always holds plain JSON.

Migration order is load-bearing and enforced explicitly in both `cmd/server/main.go` and the equivalent bootstrap: `outbox.ApplySchema` (which creates `outbox_events` with a JSONB payload column) **must** run before this service's own domain migration, which `ALTER`s that column to `text` and adds a normalization trigger (needed because PgBouncer's simple-query protocol otherwise bytea-hex-encodes JSONB parameters). Running them in the other order leaves the column JSONB and the later ALTER either fails or silently doesn't apply, depending on migration tooling.

---

## Reconciler jobs (`cmd/reconciler`)

| Job | LLD ID | Schedule | Summary |
|---|---|---|---|
| `trial-expiry` | RP-C1 | `0 2 * * *` | Sweeps its own `tenant_realms` (`strategy='shared' AND status='active' AND trial_ends_at < now()`, via `idx_tenant_realms_trial_exp`) and calls RP-12 per tenant — the cron does no inline Keycloak logic itself, it fans out to the same idempotent endpoint RP-12 exposes. Never queries Org & Membership. |
| `trial-hard-delete` | RP-C2 | `0 3 * * *` | Past `TRIAL_GRACE_DAYS` (default 30), hard-deletes the shared-realm users via the Keycloak Admin API and scrubs the local row, retaining only `tenant_id` for audit-FK purposes. O&M runs its own independent cleanup cron for its own `tenants` row — no coordination needed between the two. |
| `subscription-lapse` | RP-C3 | `0 2 * * *` | Daily sweep of dedicated tenants' billing subscription status: `cancelled` past `CANCELLED_GRACE_DAYS` → disable the dedicated realm, mark `suspended`, emit `TenantSuspended(source=billing_lapse)`; `suspended` past `RETENTION_DAYS` total → export the realm to S3 (confirmed durable write) **then** delete it, mark `exported`→`deleted`, scrub the row, emit `TenantOffboarded`. Operator-sourced suspensions (`source='operator'`, no `cancelled_at`) are excluded from the auto-offboard leg — they stay `suspended` indefinitely until an operator explicitly acts. Never deletes without a confirmed export. |
| `realm-config-reconcile` | RP-C4 | `*/10 * * * *` | Retries every `realm_config_reconciliations` row in `status='pending'` (recorded when RP-8's Keycloak apply hit a transient failure) — re-attempts the apply; success marks `applied`, failure increments `attempts` and records `last_error`. |
| `outbox-prune` | — | operational | `outbox.Runner.PrunePublished` past `OUTBOX_RETENTION_DAYS`. |
| `processed-events-prune` | — | operational | Deletes `processed_events` rows past `PROCESSED_EVENTS_TTL_DAYS`. |

### Open item — RP-C3 and the O&M subscription-status contract

`subscription-lapse` needs to read every dedicated tenant's current billing/subscription status and the relevant timestamps from Org & Membership. The frozen LLD (§8.9/§16) specifies that this read happens but does not name the concrete O&M endpoint that serves it — no bulk/listing endpoint for this purpose exists in O&M's own committed API surface as of this LLD's sign-off.

Rather than guess at an unratified cross-service contract, `internal/core/port/subscription_status_reader.go` defines the interface shape RP-C3 needs (`SubscriptionStatusReader.ListDedicatedTenantSubscriptions`) and documents the gap inline. `cmd/reconciler/main.go` wires `jctx.SubscriptionStatus = nil` until O&M ships the concrete contract; `SubscriptionLapse`'s transition logic (suspend/offboard state machine, grace-period math, the operator-suspension exclusion) is built and unit-tested against a mock implementing this interface, so wiring the real adapter later is additive — it does not require touching the reconciler's business logic.

---

## OpenBao integration

`internal/adapter/outbound/openbao/` implements a KV v2 client using the Kubernetes auth method: a pod's ServiceAccount JWT is exchanged at `/v1/auth/kubernetes/login` for a client token, cached and used as `X-Vault-Token` on subsequent reads/writes/deletes against `/v1/{mount}/data/{path}` (and `/v1/{mount}/metadata/{path}` for a full delete). IdP secrets are written/read at the frozen path `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>` — deterministic, never stored as a reference anywhere in Postgres.

**Known limitation:** this adapter has been structurally reviewed and unit-tested against a fake HTTP transport, but has not been exercised against a live OpenBao instance outside a real Kubernetes cluster — the Kubernetes ServiceAccount token file the auth flow depends on doesn't exist in local dev or in the testcontainers-based integration suite. Locally, RP-6/RP-7 (IdP configure/remove) will return `500` when they reach the OpenBao secret-write/delete step. This is an expected local-dev/CI gap, not a known code defect, and should be closed with a live OpenBao smoke test as part of staging deployment validation rather than assumed correct from unit tests alone.

---

## Shared platform libraries

| Library | Version | Purpose |
|---|---|---|
| `platform-gincommon` | v1.3.0 | HTTP middleware chain, Zap logging, OTel tracing, Prometheus metrics, `RequestContext`, graceful `Shutdown` |
| `platform-pgcommon` | v1.2.1 | pgx/v5 pool, RLS GUC injection (`GUCSetFromContext`), `RunInTx`, migrations, error helpers |
| `platform-events` | v1.4.0 | Transactional outbox, SNS publisher, SQS consumer, CloudEvents-style envelope, `WithCodec` Glue Schema Registry hook |
| `Nerzal/gocloak` | v13.9.0 | Keycloak Admin API client — imported ONLY by `internal/adapter/outbound/keycloak/` (RP-INV-1) |

---

## Session-specific decisions

A small number of judgment calls were made during implementation where the frozen LLD was silent on an internals-only detail, or where a limitation of the local dev/test environment prevented full verification. Each is documented at its point of impact in the code as well as here:

1. **RP-9's Gin route parameter name** (`internal/adapter/inbound/http/router.go`) — the LLD's §23 frozen path spells the segment `:uid`, but Gin requires every route sharing a URL prefix to use the same wildcard capture name at each path position, and RP-15/RP-16 (registered on the same `/tenants/:id/users/...` prefix) already use `:keycloak_user_id`. The capture name is an internal Gin routing detail, not part of the wire contract — callers substitute the actual Keycloak user ID regardless of what the internal variable is named — so RP-9 was registered with `:keycloak_user_id` instead of `:uid`. This is non-breaking and does not change any URL a caller sends.
2. **OpenBao live-testing limitation** — see the OpenBao Integration section above. The adapter is structurally correct and unit-tested but not live-tested outside a real Kubernetes cluster; flagged rather than silently assumed to work.
3. **RP-C3's O&M subscription-status contract** — see the Reconciler Jobs section above. Rather than invent an endpoint shape another team would then be locked into, the port interface was built to the LLD's described need and left explicitly unwired pending cross-team coordination.
