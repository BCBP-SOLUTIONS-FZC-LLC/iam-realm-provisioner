# iam-realm-provisioner

The Realm Provisioner Service — the **single writer to the Keycloak Admin API** and the system of record for tenant identity lifecycle in the IAM subsystem. It provisions and tears down Keycloak realms and users, migrates trial tenants into dedicated realms, configures per-tenant SAML/OIDC identity providers (secrets held only in OpenBao, never Postgres), and drives the tenant lifecycle state machine (trial → active → suspended → offboarded) via four scheduled reconciler jobs.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner`
**Module:** Go 1.26.6 · private module · deployed as two binaries from one image (`cmd/server`, `cmd/reconciler`)
**Design:** Refines the IAM HLD; LLD v1.0 Approved (`docs/lld/iam-lld-realm-provisioner-service.md`; condensed reference at `docs/lld/IMPLEMENTATION_REFERENCE.md`).

---

## Mental model

**RP-INV-1 (single Keycloak writer):** every mutation against the Keycloak Admin API in the entire IAM subsystem goes through this service's `KeycloakAdmin` port (`internal/core/port/keycloak_admin.go`), implemented by exactly one adapter (`internal/adapter/outbound/keycloak/`) using `Nerzal/gocloak`. No other IAM service, and no other package in this repo, is permitted to call Keycloak's admin endpoints directly — enforced in CI by a grep over every Go file (`.github/scripts/arch-lint.sh`).

**Does NOT own:** the organizational model (tenants-as-business-entities, memberships, department/role grants — Org & Membership); credentials validation at request time or JWT issuance (Keycloak itself, at runtime); delegation, tender ACL overlays, or group→role mapping config (their own sibling services). This service owns Keycloak realm/user/IdP *administration* and the small amount of local state (`tenant_realms`, IdP configs, reconciliation/idempotency ledgers) needed to make that administration safe, idempotent, and replayable.

**No secret material in Postgres, ever.** Realm passwords and trial-user seed credentials live only in Keycloak. SAML/OIDC IdP client secrets live only in OpenBao, at the deterministic path `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`.

---

## API overview

All 16 endpoints live under the mesh-only prefix `/api/v1/internal` (source of truth: `internal/adapter/inbound/http/router.go`).

| # | Method & Path | Purpose |
|---|---|---|
| RP-1 | `POST /tenants/provision-trial` | Provision a trial tenant in the shared realm |
| RP-2 | `POST /tenants/provision-direct` | Provision a direct-paid signup straight into a dedicated realm |
| RP-3 | `POST /tenants/:id/convert` | Convert a trial tenant (shared realm) to a dedicated realm, migrating its users |
| RP-4 | `POST /tenants/:id/revert-conversion` | Revert a dedicated realm back to trial-in-shared-realm within the revert window |
| RP-5 | `POST /tenants/:id/users` | Create an invited user in the tenant's realm |
| RP-6 | `PUT /tenants/:id/idp` | Configure (create or update) a SAML/OIDC IdP for a tenant |
| RP-7 | `DELETE /tenants/:id/idp/:alias` | Remove a tenant's IdP configuration |
| RP-8 | `PATCH /tenants/:id/realm-config` | Apply realm-level config (e.g. `local_accounts_enabled`) |
| RP-9 | `POST /tenants/:id/users/:keycloak_user_id/mfa-reset` | Reset a user's MFA enrollment |
| RP-10 | `POST /tenants/:id/reactivate` | Reactivate a suspended dedicated tenant |
| RP-11 | `GET /tenants/:id/realm` | Read a tenant's realm status/metadata |
| RP-12 | `POST /tenants/:id/expire-trial` | Idempotent per-tenant trial-expiry effect (called by RP-C1) |
| RP-13 | `POST /audit/idp-region-portability` | Operator-only, whole-estate IdP region-portability audit |
| RP-14 | `POST /tenants/:id/suspend` | Operator-triggered tenant suspension |
| RP-15 | `DELETE /tenants/:id/users/:keycloak_user_id` | Delete a user from a tenant's realm |
| RP-16 | `POST /tenants/:id/users/:keycloak_user_id/revoke-sessions` | Revoke a user's active sessions |

Every mutating endpoint (all except RP-11/RP-13-as-read) requires an `Idempotency-Key` header, checked against the `provisioning_operations` ledger — a retried call with the same key replays the stored result instead of re-executing (409 `operation_in_progress` if the original call is still in flight).

RP-9's Gin route uses the internal parameter name `:keycloak_user_id` rather than the LLD's literal `:uid` spelling — Gin requires one wildcard name per path position across sibling routes sharing the prefix (RP-15/16 already use `:keycloak_user_id` here). This is a routing-internals detail only; the actual URL callers hit is unchanged.

Full request/response shapes and error codes: `docs/lld/IMPLEMENTATION_REFERENCE.md` and the frozen LLD's §5/§17.

---

## Events

### Produced — `iam-tenant-events` (SNS), 9 event types

| Event | Producer | Key fields |
|---|---|---|
| `TrialTenantProvisioned` | RP-1 | `tenant_id`, `realm`, `keycloak_user_id` |
| `TenantRealmReady` | RP-2, RP-3 | `tenant_id`, `realm`, `keycloak_shard`, `oidc_clients` |
| `TenantConverted` | RP-3 | `tenant_id`, `realm`, `plan`, `billing_subscription_id`, `users_migrated`, `direct_signup` |
| `DirectPaidSignup` | RP-2 | `tenant_id`, `realm`, `direct_signup` |
| `TrialExpired` | RP-C1 / RP-12 | `tenant_id`, `trial_ends_at`, `grace_ends_at` |
| `TrialReactivated` | RP-4 | `tenant_id`, `new_trial_ends_at` |
| `TenantSuspended` | RP-C3 (`source=billing_lapse`) or RP-14 (`source=operator`) | `tenant_id`, `realm`, `cancelled_at`, `suspended_at`, `source`, `reason` |
| `TenantOffboarded` | RP-C3 | `tenant_id`, `realm`, `export_s3_key`, `offboarded_at` |
| `TenantReactivated` | RP-10 | `tenant_id`, `realm`, `source`, `reason` |

Every payload is validated against an embedded JSON Schema (`internal/eventschema/*.json`) before the outbox insert; `internal/eventschema/asyncapi_drift_test.go` asserts the AsyncAPI spec's message list and the embedded schema set never drift apart.

### Consumed — 3 SQS queues

| Queue | Filter (`EventType`) | Drives |
|---|---|---|
| `membership-realm-q` | `DepartmentMembershipGranted`, `DepartmentMembershipLevelChanged`, `TenantRoleGranted`, `TenantRoleRevoked` | `MfaService.SyncRequiresMfa` — adds `requires-mfa` on elevate/grant, drops it only on revoke after an O&M residual-check read |
| `tenant-realm-q` | `TrialStarted` | Idempotent upsert of `trial_ends_at` onto the tenant's `tenant_realms` row |
| `billing-realm-q` | `TenantReactivated` | Re-enables a `suspended` dedicated realm; no-op otherwise (still recorded in `processed_events`) |

Each queue has a `-dlq` sibling with `maxReceiveCount=5`. Dedup is via the `processed_events` table (RLS-exempt), keyed by event ID + consumer.

---

## Reconciler jobs (`cmd/reconciler`, `--job=<name>`)

| Job flag | LLD ID | Schedule | Does |
|---|---|---|---|
| `trial-expiry` | RP-C1 | `0 2 * * *` | Sweeps this service's own `tenant_realms` for expired shared-realm trials; calls RP-12 per tenant so the effect goes through the same idempotency ledger. Never queries O&M. |
| `trial-hard-delete` | RP-C2 | `0 3 * * *` | Past `TRIAL_GRACE_DAYS`, hard-deletes shared-realm trial users and scrubs the local row (retains only `tenant_id`). |
| `subscription-lapse` | RP-C3 | `0 2 * * *` | Daily sweep of dedicated tenants' billing subscription status, driving suspend-past-`CANCELLED_GRACE_DAYS` and offboard-past-`RETENTION_DAYS` (export to S3 before delete; never deletes without a confirmed export). |
| `realm-config-reconcile` | RP-C4 | `*/10 * * * *` | Retries every `realm_config_reconciliations` row in `status='pending'`. |
| `outbox-prune` | — | operational | Prunes published `outbox_events` rows past `OUTBOX_RETENTION_DAYS`. |
| `processed-events-prune` | — | operational | Prunes `processed_events` rows past `PROCESSED_EVENTS_TTL_DAYS`. |

**Open item — RP-C3:** the daily subscription-lapse sweep needs to read every dedicated tenant's billing/subscription status from Org & Membership, but the frozen LLD specifies that this read happens without naming the endpoint that serves it. `internal/core/port/subscription_status_reader.go` documents this gap and defines the shape RP-C3 needs; `jctx.SubscriptionStatus` is wired as `nil` in `cmd/reconciler/main.go` until O&M ships the concrete contract. See `ARCHITECTURE.md`.

---

## Local development

### Prerequisites

- Go 1.26.6 (must match `go.mod`)
- Docker + docker compose (Postgres, LocalStack, Keycloak)
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (private modules)
- SSH key registered with the BCBP org, or a `GO_PRIVATE_TOKEN` for HTTPS-based module resolution

### Setup

```bash
make setup       # copies .env-example -> .env
make docker-up   # starts Postgres (5544) + LocalStack (4571) + Keycloak (8091)
make run         # go run ./cmd/server — applies migrations, then serves on :8080 / :9090
```

`make docker-up`'s LocalStack container runs `scripts/init-localstack.sh` on first start, which provisions the `iam-tenant-events` SNS topic and the three inbound SQS queues (+ DLQs) with `RawMessageDelivery=true` subscriptions — required because SQS otherwise wraps the delivered body in an SNS notification envelope instead of the raw event JSON `platform-events`' consumer expects.

Keycloak comes up in `start-dev` mode (single node, in-memory store, not for production) on host port 8091, admin console credentials `admin`/`admin`.

Running a single reconciler job locally:

```bash
make run-reconciler JOB=trial-expiry
```

### Environment variables

The service fails fast (`validateRequiredEnv` in `cmd/server/main.go`) if any of `KEYCLOAK_BASE_URL`, `KEYCLOAK_ADMIN_CLIENT_SECRET`, `ORG_MEMBERSHIP_BASE_URL`, `SNS_TOPIC_TENANT_ARN`, `GLUE_REGISTRY_TENANT_NAME`, or a valid Postgres DSN is missing outside `APP_ENV=dev|development|local|test`. Full list in `.env-example`; highlights:

| Variable | Purpose |
|---|---|
| `PG_HOST`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME` or `DATABASE_URL` | App pool (RLS-enforced) |
| `SYSTEM_DATABASE_URL` | BYPASSRLS pool for the reconciler and cross-tenant sweeps — must target `realm_provisioner_migrator` in production |
| `MIGRATION_DATABASE_URL` | Direct (non-PgBouncer) DSN used to run `outbox.ApplySchema` then the domain migration at startup |
| `PG_BOUNCER_MODE` | `true` in production — switches the pool to transaction-scoped GUC injection |
| `KEYCLOAK_BASE_URL` / `KEYCLOAK_ADMIN_CLIENT_ID` / `KEYCLOAK_ADMIN_CLIENT_SECRET` | RP-INV-1's sole seam — the Keycloak Admin API credential |
| `SHARED_TRIAL_REALM` | Name of the multi-tenant shared realm trials provision into |
| `BAO_ADDR` / `BAO_AUTH_ROLE` / `BAO_KV_MOUNT` / `BAO_IDP_PATH_PREFIX` / `BAO_KEYCLOAK_ADMIN_PATH` | OpenBao KV v2 + Kubernetes-auth wiring for IdP secrets |
| `ORG_MEMBERSHIP_BASE_URL` | O&M base URL — the I-8 residual-MFA-requirement check |
| `SNS_TOPIC_TENANT_ARN` / `GLUE_REGISTRY_TENANT_NAME` | Outbound event publishing + Glue schema registry |
| `SQS_MEMBERSHIP_REALM_QUEUE_URL` / `SQS_TENANT_REALM_QUEUE_URL` / `SQS_BILLING_REALM_QUEUE_URL` | Inbound consumer queues — a consumer is disabled (warns, doesn't fail) if its URL is unset |
| `REALM_EXPORT_BUCKET` / `REALM_EXPORT_KMS_KEY` | S3 destination for RP-C3's pre-offboard realm export |
| `TRIAL_GRACE_DAYS` / `CANCELLED_GRACE_DAYS` / `RETENTION_DAYS` / `RECONCILER_BATCH_LIMIT` | Lifecycle timer knobs read by the reconciler jobs |

### Calling the API locally

```bash
curl -i -X POST http://localhost:8080/api/v1/internal/tenants/provision-trial \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "x-tenant-roles: iam-system" \
  -d '{"tenant_id":"'"$(python3 -c 'import uuid; print(uuid.uuid4())')"'","email":"owner@acme.test","full_name":"Acme Owner"}'
```

---

## Testing

| Command | Covers |
|---|---|
| `make test-unit` | Fast, no Docker — `test/unit/` + white-box package tests (`internal/core/service`, `internal/core/domain`, `internal/eventschema`, adapters) |
| `make test-postgres` | Real Postgres via testcontainers-go — the full §14.5 RLS matrix (Cases 1–5, including transaction-scoped GUC reset under simulated PgBouncer pooling) |
| `make test-integration` | Real Postgres + Keycloak 26 via testcontainers-go — realm/user/IdP/MFA operations against an actual Keycloak Admin API, including the trial→dedicated user-migration path |
| `make test-e2e` | End-to-end HTTP router + SQS consumer + reconciler flows |
| `make test-ci` | unit + postgres + integration with `-race`, merged coverage report (used in CI) |
| `make race` | Same three suites, `-race`, no coverage merge |

```bash
go test ./internal/eventschema/... -run TestAsyncAPIVsSchemaDrift -v
go test ./test/postgres/... -tags=integration -run TestRLS_Case5_NoGUCLeakageAcrossPooledConnection -v
```

Run `make help` for the complete command list (build, lint, arch-lint, migrations, coverage, docker-up/down).

---

## Further reading

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — layering, invariants, and the provisioning/lifecycle flows in detail
- [`docs/runbook.md`](docs/runbook.md) — operator runbooks
- [`CHANGELOG.md`](CHANGELOG.md)
- [`docs/lld/IMPLEMENTATION_REFERENCE.md`](docs/lld/IMPLEMENTATION_REFERENCE.md) — condensed frozen LLD reference
