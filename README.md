# iam-realm-provisioner

The Realm Provisioner Service — the **single writer to the Keycloak Admin API** (RP-INV-1) and the system of record for tenant identity lifecycle in the IAM subsystem. It provisions and tears down Keycloak realms and users, migrates trial tenants into dedicated realms, configures per-tenant SAML/OIDC identity providers (secrets held only in OpenBao, never Postgres), and drives the tenant lifecycle state machine (trial → active → suspended → offboarded) via six scheduled reconciler jobs.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner`
**Module:** Go 1.26.6 · private module · deployed as two binaries from one image (`cmd/server`, `cmd/reconciler`)
**Design:** Refines the IAM HLD (`IAM HLD v1.42`, §5.2); LLD **v1.3 Approved** — names frozen at v1.0 sign-off (`docs/lld/iam-lld-realm-provisioner-service.md`; condensed reference at `docs/lld/IMPLEMENTATION_REFERENCE.md`)

---

## Mental model

| This service owns | It does NOT own |
|---|---|
| `tenant_realms` — one row per tenant: realm name/strategy (`shared`\|`dedicated`), Keycloak shard, lifecycle status, trial/conversion/disable/export timestamps, `record_version` | The organizational/business model — tenants-as-business-entities, memberships, department/role grants, plan/subscription state (Org & Membership) |
| `keycloak_idp_configs` — per-tenant SAML/OIDC IdP configuration, **non-secret** params only | Credentials validation at request time or JWT issuance (Keycloak itself, at runtime) |
| `keycloak_idp_config_validations` — append-only §11.6 region-portability validation history + admin acknowledgements | Delegation, tender ACL overlays, or group→role mapping config (their own sibling services) |
| `realm_config_reconciliations` — RP-C4's retry ledger for realm-config settings that failed to apply inline | Any per-user table — there is none; Keycloak is the sole store of user identity/credentials |
| `provisioning_operations` — the idempotency ledger keyed by the caller-supplied `Idempotency-Key` | Secret material of any kind in Postgres — realm passwords and trial-user seed credentials live only in Keycloak; IdP client secrets live only in OpenBao |
| **The Keycloak Admin API itself** — every mutation against Keycloak in the entire IAM subsystem goes through this service's `KeycloakAdmin` port, one adapter, `Nerzal/gocloak` | — |

Org & Membership owns the organizational/business model and calls this service over the mesh; this service owns Keycloak realm/user/IdP **administration** and the small amount of local state needed to make that administration safe, idempotent, and replayable.

**Two ordering disciplines, used per-flow (§9.1):**
1. **Keycloak-effect-first, then local commit** — the default for every provisioning/migration/IdP/MFA/realm-config-apply-success flow. The Keycloak mutation runs entirely outside any Postgres transaction; only on success does `RunInTx` commit the local row **and** enqueue the outbox event(s) atomically. A crash between the two is recovered by idempotent replay of the same `Idempotency-Key` — the Keycloak step becomes a no-op reconcile, then the local commit lands. A committed local row therefore always reflects a real Keycloak state, never the reverse.
2. **Local-intent-first, then reconcile** — used only for RP-8 realm-config when Keycloak is transiently unreachable: the desired value is recorded `pending` in `realm_config_reconciliations` and the caller gets `200 {applied:false, reconciling:true}` (not an error), and RP-C4 converges it later.

---

## Why this service exists

Keycloak's Admin API is a shared, tenant-spanning administrative surface — every realm, every user, every IdP config, every MFA credential in the platform. If more than one IAM service held a Keycloak admin credential, two services could race to create the same realm, disagree about a realm's naming convention, or leak an admin token through a wider blast radius than necessary. Centralizing every Keycloak mutation behind one process (RP-INV-1) means:

- There is exactly one holder of the Keycloak admin credential in the entire platform — one OpenBao policy to audit, one place a compromised credential can do damage.
- Idempotency and Keycloak-effect-first ordering are enforced once, in one codebase, rather than re-implemented (and potentially re-broken) per caller.
- Downstream services (Org & Membership, Billing, Signup, AuthZ Enrichment) integrate against a stable HTTP/event contract instead of Keycloak's own admin REST API and its version-specific quirks.
- The tenant lifecycle state machine (trial → active → suspended → offboarded) has one authoritative implementation, so "is this tenant's realm actually live in Keycloak right now" always has one correct answer.

This is enforced structurally, not just by convention: `internal/adapter/outbound/keycloak/` is its own `go-arch-lint` component, and CI (`.github/scripts/arch-lint.sh`) greps every Go file in the repository for a `Nerzal/gocloak` import outside that one package.

---

## API overview

All 16 endpoints (RP-1..RP-16) live under one mesh-internal prefix, `/api/v1/internal/*` — this service has almost no public surface. `/healthz`/`/readyz` and the `/asyncapi`/`/asyncapi.yaml` docs surface sit outside that group, unauthenticated. `/metrics` is served on its **own** port (`METRICS_PORT=9090`), never on the main router. Source of truth: `internal/adapter/inbound/http/router.go`.

**Middleware chain** (`registerInternalRoutes`): 1 MB body-size cap → `gincommon.TimeoutMiddleware(30s)` → `gincommon.ObservabilityMiddlewares` → `gincommon.ProtectedMiddlewares` (`RequireAuth` + `ContextMiddleware`) → `GUCBridgeMiddleware` (binds `app.tenant_id` transaction-locally, RLS-6) → `RequireJSONContentType` → `RequireSystemRole` (defense-in-depth — mesh mTLS + NetworkPolicy is primary) → per-route `RequireIdempotencyKey` / `RequireOperatorRole`.

Every mutating endpoint below marked "required" enforces `RequireIdempotencyKey`: a missing header is `400 missing_idempotency_key`; a reused key against an in-flight/failed operation is `409 operation_in_progress`; a reused key against a `succeeded` operation replays the stored JSON result rather than re-executing.

### Internal routes (system-authenticated, 14)

| # | Method & Path | Idempotency-Key | Purpose |
|---|---|---|---|
| RP-1 | `POST /tenants/provision-trial` | required | Create a trial user in the shared `trial` realm, stamp `tenant_id`, trigger `VERIFY_EMAIL`. Emits `TrialTenantProvisioned`. |
| RP-2 | `POST /tenants/provision-direct` | required | Provision a **dedicated** realm straight away: create realm from template, register OIDC clients, create admin user. Emits `TenantRealmReady` + `DirectPaidSignup` + `TenantConverted{direct_signup:true}`. |
| RP-3 | `POST /tenants/:id/convert` | required | Trial → dedicated conversion: create dedicated realm, migrate every trial user (`sub`-preserving, TOTP copied, WebAuthn re-enrollment flagged), force-logout the shared realm. Emits `TenantRealmReady` + `TenantConverted{direct_signup:false}`. |
| RP-4 | `POST /tenants/:id/revert-conversion` | required | Undo Convert within a fixed reversal window. `409 revert_window_expired` past the window. Emits `TrialReactivated`. |
| RP-5 | `POST /tenants/:id/users` | required | Create an invited user in the tenant's current realm with `required_actions` applied verbatim. No local persistence. |
| RP-6 | `PUT /tenants/:id/idp` | required | Configure a SAML/OIDC IdP behind the §11.6 region-portability gate. Requires a `dedicated` realm (`409 idp_requires_dedicated_realm` otherwise). |
| RP-7 | `DELETE /tenants/:id/idp/:alias` | — | Remove an IdP — Keycloak, then the OpenBao secret, then the local row. Idempotent. |
| RP-8 | `PATCH /tenants/:id/realm-config` | — | Apply an allow-listed realm-level setting; on transient Keycloak failure, records a `pending` reconciliation row instead of failing the caller (RP-INV-6, local-intent-first). |
| RP-9 | `POST /tenants/:id/users/:keycloak_user_id/mfa-reset` | — | Delete every OTP/WebAuthn credential; re-adds `CONFIGURE_TOTP` if the user still holds `requires-mfa`. Fail-closed, no reconciler. |
| RP-10 | `POST /tenants/:id/reactivate` | required | Re-enable a disabled trial user or a suspended dedicated realm — reverses **both** a billing-lapse and an operator suspension from one endpoint. `409 tenant_offboarded` if terminal. |
| RP-11 | `GET /tenants/:id/realm` | — | Read-only: current realm name/strategy/shard/status. |
| RP-12 | `POST /tenants/:id/expire-trial` | required | Idempotent per-tenant trial-expiry effect; called once per row by RP-C1, also directly callable. |
| RP-15 | `DELETE /tenants/:id/users/:keycloak_user_id` | — | Idempotent Keycloak user delete — deleting an absent user is a success. |
| RP-16 | `POST /tenants/:id/users/:keycloak_user_id/revoke-sessions` | — | Idempotent, best-effort/fail-open session/token revocation. |

### Operator routes (2)

| # | Method & Path | Idempotency-Key | Purpose |
|---|---|---|---|
| RP-13 | `POST /audit/idp-region-portability` | — | Whole-estate, **not tenant-scoped** — re-validates every stored IdP config's non-secret `Config` against the §11.6 checklist and returns pass/warnings/failing counts + the failing tenant IDs. |
| RP-14 | `POST /tenants/:id/suspend` | required | Emergency operator-triggered lockout — disables the dedicated realm (or just that tenant's shared-realm users). Effect-only; never touches subscription-status fields. Reversible via RP-10. Emits `TenantSuspended{source:"operator"}`. |

RP-13 and RP-14 additionally require `RequireOperatorRole()` — `platform_operator` in `x-tenant-roles` — checked at the handler as defense-in-depth on top of network isolation.

**RP-9's route param note:** the frozen §25 path spells the segment `:uid`, but Gin requires one wildcard capture name per path position across sibling routes sharing a prefix, and RP-15/RP-16 already use `:keycloak_user_id` there — so RP-9 was registered with `:keycloak_user_id` too. Internal routing detail only; the URL a caller hits is unchanged.

---

## Input validation

Unlike a collect-all-errors service, this service is **fail-fast, single-error**: Gin's `ShouldBindJSON` stops at the first failing `binding` tag, and a bind failure is wrapped into one `400 invalid_request` response — never a field-level array. Business-rule failures (invalid enum values, allow-list misses, the §11.6 IdP checklist) are raised as a single named `domain.Error` from `internal/core/domain/errors.go`, each mapped to a frozen HTTP status via `Error.Status()`:

```json
{
  "error": "invalid_idp_protocol",
  "status": 400,
  "trace_id": "a3f1b2c8d4e5f60718293a4b5c6d7e8f",
  "request_id": "01H8XYZ...",
  "details": { "reason": "..." }
}
```

`internal/adapter/inbound/http/errors.go`'s `HandleError` does the mapping; anything not a `*domain.Error` falls back to `500 internal_error` and is logged so no error path is silent.

### Notable validation rules

| Field | Rule (as implemented) | Error / code |
|---|---|---|
| `tenant_id` | `binding:"required"`, parsed as `uuid.UUID` — a malformed UUID fails Gin binding | `400 invalid_request` |
| `tenant_name` | `binding:"required"` (string, no further format check in this repo) | `400 invalid_request` |
| `tenant_slug` | `binding:"required"` — no server-side charset/length validator exists in this codebase; an invalid Keycloak realm name is rejected by Keycloak itself at `CreateRealmFromTemplate` | `400 invalid_request` (missing) / `502 keycloak_unavailable` (Keycloak rejects the name) |
| `admin_email` | `binding:"required,email"` (Gin/go-playground validator's `email` tag) | `400 invalid_request` |
| `plan` | `binding:"required"` (string, no enum check in this repo) | `400 invalid_request` |
| `reason` | `binding:"required"` on RP-14 (`suspendRequest`) only; optional/free-text elsewhere (RP-4 `revertConversionRequest`, RP-10 `reactivateRequest`) | `400 invalid_request` (RP-14 only) |
| `idp_alias` (RP-6 body / RP-7 path param) | `binding:"required"` at the HTTP layer, then `IdpService` enforces `^[a-zA-Z0-9_-]{1,64}$` before any OpenBao/Keycloak call — no dot permitted at all, closing a path-traversal risk since `openbao.Client.idpPath` interpolates this value directly into the KV path | `400 invalid_request` |
| `protocol` | `binding:"required"`; must be `saml` or `oidc` (`domain.IdpProtocol`), checked in `IdpService` | `400 invalid_request` (missing) / `400 invalid_idp_protocol` (bad value) |
| `setting_key` | Checked against `domain.IsAllowedSettingKey` — an exact match in `AllowedRealmSettingKeys` (`local_accounts_enabled`) or an `idp:<alias>` federation-toggle prefix | `400 unsupported_realm_setting` |
| `Idempotency-Key` (header) | Non-empty, ≤ 256 chars, enforced by `RequireIdempotencyKey` on every route so marked above | `400 missing_idempotency_key` |
| `Content-Type` (header) | Must be `application/json` on a non-empty POST/PUT/PATCH body, enforced by `RequireJSONContentType` | `415 unsupported_media_type` |

The §11.6 Region-Agnostic IdP Configuration Checklist (`internal/core/service/idp_validator.go`) is a separate, business-level validation pass on RP-6: CRITICAL checks (region-pinned SSO/SLO URLs, missing signing cert, non-neutral ACS/issuer/redirect/audience) **block** with `400 idp_region_portability_failed`; WARN checks (weak `NameIDFormat`, region-bearing attribute-mapper URNs, a signature algorithm below `RSA-SHA256`, missing OIDC PKCE `S256`) require a recorded acknowledgement or return `422 idp_warnings_unacknowledged`. Every run — pass or fail — is recorded in the append-only `keycloak_idp_config_validations` table.

Validation-relevant fields are never logged with their raw values in error logs — only `tenant_id`, `operation`, `idempotency_key`, `keycloak_realm`, `actor`.

---

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers: `domain` ← `port` ← `service` ← `adapter` ← `cmd`. Two composition roots ship from one Docker image: `cmd/server` (HTTP API + 3 SQS consumers + outbox runner) and `cmd/reconciler` (CronJob dispatcher, `--job=<name>`). Full layer diagrams, package dependency graph, request/write/provisioning flow sequence diagrams, cache strategy, RLS/GUC flow, and the threat model are in **[ARCHITECTURE.md](ARCHITECTURE.md)**.

```
iam-realm-provisioner/
├── cmd/
│   ├── server/                        # HTTP composition root: pool+GUC wiring, migrations, outbox runner, 3 SQS consumers
│   └── reconciler/                    # Single binary, --job=<name>; jobs/ holds the 6 CronJob entry points
├── internal/
│   ├── core/
│   │   ├── domain/                    # Entities, value objects, ErrorCode taxonomy (§17), event payload structs — no deps beyond google/uuid
│   │   ├── port/                      # KeycloakAdmin (RP-INV-1's sole seam), repositories, IdempotencyStore, ProcessedEventsStore, OpenBaoClient, OrgMembershipClient, ObjectStore, SubscriptionLapseReader
│   │   └── service/                   # ProvisioningService, LifecycleService, IdpService, RealmConfigService, MfaService
│   ├── eventschema/                   # Embedded JSON Schema draft-07, one per produced event + asyncapi_drift_test.go
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                  # Gin handlers (RP-1..RP-16), DTOs, middleware, router.go, /asyncapi
│       │   └── consumer/              # 3 SQS consumers: membership-realm-q, tenant-realm-q, billing-realm-q
│       └── outbound/
│           ├── keycloak/              # gocloak-backed KeycloakAdmin — RP-INV-1's ONLY implementation
│           ├── postgres/              # Repository impls + single consolidated 000000_initial_schema migration
│           ├── eventbus/              # Outbox publisher (single topic iam-tenant-events) + Glue/Noop/Validating codecs
│           ├── openbao/               # KV v2 client, Kubernetes-auth (pod ServiceAccount → OpenBao role → policy)
│           ├── orgmembership/         # HTTP client for O&M's I-8 residual-MFA check + I-16 subscription-lapse poll (RP-C3)
│           ├── s3/                    # Realm-export object store (offboarding, RP-EVT-4)
│           ├── httpx/                 # Shared instrumented http.RoundTripper (traceparent injection)
│           └── metrics/               # iam_realm_provisioner_* Prometheus counters/histograms/gauges
├── pkg/requestctx/                    # Typed RequestContext{UserID, TenantID, Roles}
├── api/
│   ├── asyncapi.yaml                  # AsyncAPI 3.0 — iam-tenant-events (9 published) + 3 inbound SQS channels
│   └── embed.go                       # //go:embed asyncapi.yaml — served from the compiled binary, no source tree in the image
├── deploy/                            # Helm chart, AWS IAM policy, OpenBao policy, Prometheus alerts, Keycloak realm template
├── docs/lld/                          # Frozen LLD v1.3 + condensed IMPLEMENTATION_REFERENCE.md
├── docs/runbook-schema-registry.md    # Schema-governance operator runbook (shared iam-tenant-events registry)
├── .githooks/pre-commit               # tidy + fmt-check + vet + lint; installed via `make setup`/`make install-hooks`
├── VERSIONING.md                      # SemVer policy + maintainer release process
└── test/                              # test/unit (black-box), plus white-box tests colocated in internal/; test/postgres (RLS), test/integration, test/e2e, test/load, test/dbseed
```

### Dependency rules (enforced by `go-arch-lint` in CI)

| Package | May import |
|---|---|
| `core/domain` | Nothing internal (`anyVendorDeps: true` for `google/uuid`) |
| `core/port` | `core/domain` only |
| `core/service` | `core/domain`, `core/port`, `pkg/requestctx`, and `internal/adapter/outbound/metrics` (a documented cross-cutting observability leaf) |
| `keycloak` (own component) | `core/domain`, `core/port`, `observability`, `httptransport` — no other `adapters_outbound` file may import it |
| `adapters_outbound` (postgres/eventbus/openbao/orgmembership/s3) | `core/domain`, `core/port`, `core/service`, `eventschema`, `observability`, `httptransport` |
| `adapters_inbound` (http/consumer) | `core/domain`, `core/port`, `core/service`, `requestctx`, `apispec`, `observability` |
| `cmd` (server/reconciler) | Everything above |

Plus two extra CI-enforced invariants (`.github/scripts/arch-lint.sh`) beyond the layering check itself:
- **RP-INV-1** — `grep -rl "Nerzal/gocloak" --include="*.go" .` outside `internal/adapter/outbound/keycloak/` must be empty.
- **RLS-6** — `grep -rn "SET app\.tenant_id"` across `*.go`/`*.sql` must only ever appear as `SET LOCAL` (transaction-scoped), never a session-scoped `SET`.

### Storage and messaging

| Concern | Technology | Notes |
|---|---|---|
| **Primary store** | PostgreSQL, database `provisioner` on shared RDS (Multi-AZ, PgBouncer transaction pooling in prod) | `FORCE ROW LEVEL SECURITY` on the 5 tenant-scoped tables; GUC `app.tenant_id` bound transaction-locally |
| **Events** | AWS SNS + transactional outbox (`platform-events`) | One topic, `iam-tenant-events` — **not** a `RoutingPublisher`; this service is a co-producer with Org & Membership, disjoint event names |
| **Inbound queues** | AWS SQS, 3 queues | `membership-realm-q`, `tenant-realm-q`, `billing-realm-q`, each with its own `-dlq` (`maxReceiveCount=5`), dedup via `processed_events` |
| **Secrets** | OpenBao KV v2 | Per-tenant IdP secrets at `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`; Kubernetes auth (pod ServiceAccount token → role → policy) |
| **Realm exports** | AWS S3 (KMS-encrypted) | Offboarding — export-before-delete invariant (RP-EVT-4) |
| **Admin-token cache** | In-process, single-flight | LLD §6.2 specifies a shared Valkey tier (`rp:kcadmin:token`); not implemented in this repo — correctness unaffected, only the token-endpoint call rate rises per replica (RP-D4) |

### Shared library dependencies

| Library | Version | Purpose |
|---|---|---|
| `platform-gincommon` | v1.3.0 | HTTP middleware, Zap logging, OTel tracing, Prometheus metrics |
| `platform-events` | v1.4.0 | Transactional outbox, SNS publisher, SQS consumer |
| `platform-pgcommon` | v1.3.0 | pgx/v5 pool, RLS GUC injection, migrations, error helpers (`IsConnectionException`/`IsInsufficientResources`/`IsPgError`) |
| `Nerzal/gocloak/v13` | v13.9.0 | Keycloak Admin API client — used **only** inside `internal/adapter/outbound/keycloak/` (RP-INV-1) |

---

## Integrating with other services

This section is for platform service authors calling this service, or subscribing to its events.

### 1. Prerequisites

Every caller must:

1. Run on the **internal service mesh** (mTLS + NetworkPolicy). There is no public route to `/api/v1/internal/*`.
2. Forward `x-user-id`, `x-tenant-id`, and `x-tenant-roles` headers, gateway/mesh-injected after mTLS authentication — this service checks `rc.IsSystem()` (`iam-system` role) on every internal route and `rc.IsOperator()` (`platform_operator` role) additionally on RP-13/RP-14.
3. Send an `Idempotency-Key` header (non-empty, ≤ 256 chars) on every route marked "required" in the API overview table above.

### 2. HTTP client setup

Use `platform-gincommon`'s header propagation so trace context and identity headers survive the hop; this service's own outbound calls (to Keycloak, O&M, OpenBao) are built the same way via `internal/adapter/outbound/httpx`, which injects a W3C `traceparent` on every request.

```go
req, err := http.NewRequestWithContext(ctx, http.MethodPost,
    baseURL+"/api/v1/internal/tenants/provision-trial", bytes.NewReader(body))
req.Header.Set("Content-Type", "application/json")
req.Header.Set("Idempotency-Key", idempotencyKey)
req.Header.Set("x-user-id", systemPrincipalUserID) // 00000000-0000-0000-0000-0000000000a1
req.Header.Set("x-tenant-id", tenantID.String())
req.Header.Set("x-tenant-roles", "iam-system")
```

### 3. Signup Service — RP-1 / RP-2

Signup calls RP-1 (`provision-trial`) for a self-serve trial signup, or RP-2 (`provision-direct`) for a direct-paid signup that skips the shared realm entirely. Both are Keycloak-effect-first: a `502 keycloak_unavailable` means no local row was written — retry with the **same** `Idempotency-Key`, never a fresh one, so the retry replays cleanly once Keycloak recovers.

### 4. Billing — RP-3 / RP-4 / reactivate

Billing drives conversion, reversal, and (non-operator-sourced) reactivation over mesh calls, not events: RP-3 `convert`, RP-4 `revert-conversion`, RP-10 `reactivate`. Billing also **produces** `TenantReactivated` on its own `billing.events` topic, consumed by `billing-realm-q` — a re-enable of a suspended dedicated realm — which is a different topic/producer from this service's own `TenantReactivated` (`source=operator`) on `iam.tenant.events`; the two are distinguished by `source`, never self-consumed.

### 5. Org & Membership — multiple call sites

O&M is this service's primary caller, in both directions:

| Direction | Call | Notes |
|---|---|---|
| O&M → RP | `CreateInvitedUser` → RP-5 | No local persistence on this side |
| O&M → RP | `DeleteUser` → RP-15 | Idempotent — absent-user delete is a success |
| O&M → RP | `PatchRealmConfig` → RP-8 | Local-intent-first — a `200` does not always mean the Keycloak effect landed yet |
| O&M → RP | `RevokeUserSessions` → RP-16 | Best-effort/fail-open from O&M's perspective; RP itself stays fail-closed at the Keycloak call |
| O&M → RP | `ResetMFA` → RP-9 | Fail-closed, no reconciler — unlike RP-16 |
| RP → O&M | `OrgMembershipClient.HasResidualMFARequirement` | Backs the `membership-realm-q` MFA-revoke decision (§ below); read-only |
| O&M → events → RP | `TrialStarted` on `tenant-realm-q` | Idempotent upsert of `trial_ends_at` only — RP never queries O&M for trial timing, it's push-only |
| RP → events → O&M | All 9 produced events on `iam.tenant.events` | O&M consumes on its own `tenant-orgm-q` |

### 6. AuthZ Enrichment — event-only

AuthZ Enrichment does not call this service directly; it subscribes to `TenantConverted` on `iam.tenant.events` to maintain its realm→tenant map. See §7 below for the general subscription pattern.

### 7. Subscribing to SNS events

This service publishes to **`iam-tenant-events`** via a transactional outbox — 9 event types, envelope `source = "iam-realm-provisioner"`. `tenant_id` is on the **envelope**, never duplicated inside `data`.

| Event | Producer | Key payload fields |
|---|---|---|
| `TrialTenantProvisioned` | RP-1 | `tenant_id`, `realm`, `keycloak_user_id` |
| `TenantRealmReady` | RP-2, RP-3 | `tenant_id`, `realm`, `keycloak_shard`, `oidc_clients[]` |
| `TenantConverted` | RP-2 (`direct_signup:true`), RP-3 (`false`) | `tenant_id`, `realm`, `plan`, `billing_subscription_id?`, `users_migrated`, `direct_signup` |
| `DirectPaidSignup` | RP-2 | `tenant_id`, `realm`, `direct_signup` |
| `TrialExpired` | RP-C1 / RP-12 | `tenant_id`, `trial_ends_at`, `grace_ends_at` |
| `TrialReactivated` | RP-4, RP-10 | `tenant_id`, `new_trial_ends_at` |
| `TenantSuspended` | RP-C3 (`source:"billing_lapse"`), RP-14 (`source:"operator"`) | `tenant_id`, `realm`, `cancelled_at?`, `suspended_at`, `source`, `reason?` |
| `TenantOffboarded` | RP-C3 | `tenant_id`, `realm`, `export_s3_key`, `offboarded_at` |
| `TenantReactivated` | RP-10 | `tenant_id`, `realm`, `source`, `reason?` |

**Envelope shape:**

```json
{
  "id": "01926f3a-...",
  "type": "TrialTenantProvisioned",
  "source": "iam-realm-provisioner",
  "tenant_id": "f0e1d2c3-...",
  "subject": "a1b2c3d4-...",
  "specversion": "1",
  "time": "2026-09-01T10:30:00Z",
  "actor": "00000000-0000-0000-0000-0000000000a1",
  "dataschema": "...",
  "data": { "tenant_id": "f0e1d2c3-...", "realm": "trial", "keycloak_user_id": "a1b2c3d4-..." }
}
```

**Co-producer note:** Org & Membership also publishes on this same topic (`TenantCreated`/`TrialStarted`) — disjoint event names, no collision risk (RP-D5). Handle an unrecognized `type` gracefully (log + skip). **Idempotency:** record the envelope `id` (UUID v7) against your own consumer name before committing any side effect, mirroring this service's own `processed_events` composite PK `(event_id, consumer)` pattern — delivery is at-least-once. **Ordering:** do not assume SNS preserves delivery order; handle via upsert-style projections.

### 8. Handling errors

Every non-2xx response is the flat `ErrorResponse` shape (`internal/adapter/inbound/http/errors.go`) — `{error, status, trace_id, request_id, details?}`. There is no separate field-level validation-error shape; a bind failure is `400 invalid_request` with the same flat envelope.

```go
var errResp struct {
    Error     string         `json:"error"`
    Status    int            `json:"status"`
    TraceID   string         `json:"trace_id"`
    RequestID string         `json:"request_id"`
    Details   map[string]any `json:"details"`
}
_ = json.NewDecoder(resp.Body).Decode(&errResp)
// errResp.TraceID links to the distributed trace
```

| Code | Status | Trigger |
|---|---|---|
| `invalid_request` | 400 | Malformed JSON / failed binding validation (additive, not in the frozen §17 list) |
| `invalid_realm_strategy` / `invalid_idp_protocol` / `unsupported_realm_setting` / `missing_idempotency_key` | 400 | Named business-rule failure |
| `idp_region_portability_failed` | 400 | RP-6 — one or more §11.6 CRITICAL checks failed |
| `idp_warnings_unacknowledged` | 422 | RP-6 — WARN check(s) lack a recorded acknowledgement |
| `tenant_realm_not_found` / `idp_config_not_found` | 404 | Resource missing (RP-15/RP-16 treat a missing Keycloak user as **204**, not 404) |
| `idp_requires_dedicated_realm` / `realm_already_dedicated` / `revert_window_expired` / `tenant_offboarded` / `operation_in_progress` / `optimistic_lock_conflict` | 409 | State-machine / idempotency conflict |
| `keycloak_unavailable` / `conversion_failed` | 502 | Keycloak transport failure or partial-migration failure — retryable, no local commit |
| `org_membership_unavailable` / `object_store_unavailable` | 503 | O&M I-8 check or S3 export unreachable |

### 9. Optimistic locking

`tenant_realms` and `keycloak_idp_configs` carry `record_version`, bumped by a `touch_row()` DB trigger on any real change (a no-op update never advances it). A `409 optimistic_lock_conflict` returns the current `record_version` in `details` so the caller can re-read and retry — there is no client-supplied `expected_version` field on the current write endpoints; conflicts surface from concurrent writers hitting the same row, not from a caller-echoed version.

### 10. Rate limits

**This service applies no inbound rate limiting of its own** — no per-caller or per-endpoint token bucket exists anywhere in `internal/adapter/inbound/http/` (verified: `middleware.go` carries only `GUCBridgeMiddleware`, `RequireJSONContentType`, `RequireSystemRole`, `RequireOperatorRole`, `RequireIdempotencyKey` — no rate limiter). Inbound throttling, if any, is the API gateway's concern.

The only rate limiter in this codebase is **outbound**, client-side, against Keycloak: `KEYCLOAK_RATE_LIMIT_RPS` (default `20`), a token-bucket limiter in `internal/adapter/outbound/keycloak/` whose `Wait()` **blocks** rather than rejecting — it smooths a mass-conversion burst instead of failing it. This limiter, not replica count, is the real throughput ceiling during a large migration.

### 11. Background reconcilers you may observe

Six CronJobs, one Helm manifest (`deploy/helm/iam-realm-provisioner/templates/cronjobs.yaml`), each `cmd/reconciler --job=<name>` against the same image:

| CronJob (`--job=`) | Schedule | Purpose |
|---|---|---|
| `trial-expiry` | `0 2 * * *` | RP-C1: sweep `tenant_realms WHERE strategy='shared' AND status='active' AND trial_ends_at < now()` — **never queries O&M**. Calls RP-12's idempotent effect per tenant. |
| `trial-hard-delete` | `0 3 * * *` | RP-C2: past `TRIAL_GRACE_DAYS` beyond expiry, hard-delete trial users from Keycloak and scrub the local row. |
| `subscription-lapse` | `0 2 * * *` | RP-C3: `cancelled → suspended → offboarded`, in two phases sharing one `RECONCILER_BATCH_LIMIT` budget — suspend polls O&M's I-16 lapse list (skipped with a WARN if `ORG_MEMBERSHIP_BASE_URL` is unset), offboard always runs against locally-tracked `suspension_source='billing_lapse'` rows past `RETENTION_DAYS`. |
| `realm-config-reconcile` | `*/10 * * * *` | RP-C4: retries every `realm_config_reconciliations` row in `status='pending'`; a row past `RECONCILER_MAX_ATTEMPTS` (default 20) moves to a terminal `failed` status instead of retrying forever (poison-pill cap). |
| `outbox-prune` | `0 4 * * *` | Prunes published `outbox_events` past `OUTBOX_RETENTION_DAYS`. |
| `processed-events-prune` | `30 4 * * *` | Prunes `processed_events` rows past `PROCESSED_EVENTS_TTL_DAYS`. |

Every job is idempotent per tenant (safe to re-run) and batched (`RECONCILER_BATCH_LIMIT`, default 100).

---

## Local development

### Prerequisites

- Go 1.26.6+
- Docker (Postgres, LocalStack, Keycloak, OpenBao — `make docker-up`)
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (`GONOSUMDB` too) and an SSH key registered with the BCBP org

### Setup

```bash
git clone https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner
cd iam-realm-provisioner
make setup       # copies .env-example → .env; installs .githooks/pre-commit (run once before anything else)
make tidy        # go mod tidy
make docker-up   # start Postgres + LocalStack + Keycloak + OpenBao
make migrate-up  # apply pending migrations (the server also self-migrates at startup)
make run         # start the server on :8080 (metrics on :9090)
```

### Common commands

| Command | Description |
|---|---|
| `make setup` | Copy `.env-example` → `.env`; install `.githooks/pre-commit` |
| `make install-hooks` | Install `.githooks/pre-commit` (tidy + fmt-check + vet + lint); also run by `make setup` |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics; `fmt-check` mirrors CI, does not modify files |
| `make lint` | `golangci-lint` via `go tool` |
| `make arch-lint` | `go-arch-lint` dependency-rule enforcement + RP-INV-1/RLS-6 greps |
| `make mod-verify` | `go mod verify` |
| `make vuln-check` | `govulncheck ./internal/... ./pkg/...` |
| `make test` | Unit + postgres + integration in parallel (Docker required) |
| `make test-ci` | Same, with `-race` + merged coverage (used in CI) |
| `make test-unit` | Unit tests only, no Docker |
| `make test-postgres` | Postgres + RLS integration (testcontainers-go) |
| `make test-integration` | Cross-layer (SNS/SQS/Keycloak/OpenBao via testcontainers) |
| `make test-e2e` | End-to-end tests |
| `make race` | All three suites with `-race`, no coverage merge |
| `make run` | Run the server locally (kills `APP_PORT` first, sources `.env`) |
| `make run-reconciler JOB=<name>` | Run one reconciler job locally, e.g. `JOB=trial-expiry` |
| `make build` | Compile both binaries to `bin/` |
| `make cover` / `make cover-func` | Coverage HTML report / per-function summary |
| `make ci` | `tidy` + `fmt-check` + `vet` + `lint` + `arch-lint` + `test-ci` + `build` |
| `make docker-up` / `make docker-down` | Start/stop Postgres + LocalStack + Keycloak + OpenBao |
| `make migrate-up` / `make migrate-down` / `make migrate-create NAME=<x>` | Migration helpers (standalone `golang-migrate` CLI; the server self-migrates at startup) |
| `make clean` | Remove `bin/` and coverage artefacts |
| `make schema-pull` | Pull the `platform-schemagov` Docker image (`0.4`) |
| `make extract-schemas` | Derive `internal/eventschema/*.json` from `api/asyncapi.yaml` |
| `make schema-validate` | Validate AsyncAPI + this service's 9 event schemas — 8 passes, no AWS credentials needed |
| `make schema-diff CURRENT=… PROPOSED=…` | Show compatibility diff between two schema files |
| `make schema-register` | Register this service's 9 event schemas to the shared Glue registry (requires `GLUE_REGISTRY_TENANT_NAME`) |
| `make schema-verify` | Pre-deploy check that all 9 of this service's schemas exist in the target registry |
| `make schema-prune` | Dry-run: list orphaned Glue schemas (add `EXECUTE=true` to delete — see `docs/runbook-schema-registry.md` for the shared-registry caveat with `iam-org-membership`) |

### Running a single test

```bash
go test ./internal/core/service/... -run TestProvisionTrial -v
go test ./test/postgres/... -tags=integration -run TestRLS_Case5_NoGUCLeakageAcrossPooledConnection -v
go test ./internal/eventschema/... -run TestAsyncAPIVsSchemaDrift -v
```

### Calling the API locally

```bash
# After make docker-up and make run:

# RP-1: provision a trial tenant
curl -i -X POST http://localhost:8080/api/v1/internal/tenants/provision-trial \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $(python3 -c 'import uuid; print(uuid.uuid4())')" \
  -H "x-tenant-roles: iam-system" \
  -d '{"tenant_id":"'"$(python3 -c 'import uuid; print(uuid.uuid4())')"'","tenant_name":"Acme Corp","tenant_slug":"acme","admin_email":"owner@acme.test","admin_name":"Acme Owner","plan":"trial"}'

# RP-11: read realm status
TENANT_ID=<uuid-from-above>
curl -i http://localhost:8080/api/v1/internal/tenants/$TENANT_ID/realm \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" \
  -H "x-tenant-roles: iam-system"

# RP-14: operator suspend (requires platform_operator)
curl -i -X POST http://localhost:8080/api/v1/internal/tenants/$TENANT_ID/suspend \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" \
  -H "x-tenant-roles: iam-system,platform_operator" \
  -d '{"reason":"fraud investigation"}'
```

### Developer tools

This service has **no Swagger UI** — the REST contract is the LLD endpoint catalogue plus `router.go`, not a generated OpenAPI spec. The only documentation surface is the AsyncAPI event-contract viewer (`registerDocsRoutes`, `internal/adapter/inbound/http/router.go`):

| Handler | Route | Gating | Purpose |
|---|---|---|---|
| `AsyncAPIHandler` | `GET /asyncapi` | `DocsConfig.active()` | Server-side HTML renderer for `api/asyncapi.yaml` |
| `AsyncAPIYAMLHandler` | `GET /asyncapi.yaml` | `DocsConfig.active()` | Serves the raw embedded spec bytes |

`DocsConfig.active()` is `Environment != "production" || Enabled` — always mounted outside production; in production it's opt-in via `DOCS_ENABLED` and, if `DOCS_AUTH_TOKEN` is set, additionally bearer-token-gated. Both routes always carry `X-Frame-Options: DENY` and `X-Content-Type-Options: nosniff`. The spec is compiled into the binary via `//go:embed asyncapi.yaml` (`api/embed.go`), not read from disk — the distroless runtime image never copies the source tree in.

```bash
open http://localhost:8080/asyncapi
```

---

## Testing domain events locally

Every mutating provisioning/lifecycle call publishes a domain event through a **transactional outbox → SNS → SQS** pipeline.

### How the pipeline works

```
HTTP write / reconciler job
    │
    ▼
service layer  ──(same tx)──▶  outbox_events (Postgres)
                                      │
                               outbox runner (OUTBOX_POLL_INTERVAL, 500 ms)
                                      │
                                      ▼
                               SNS topic: iam-tenant-events        (LocalStack)
                                      │
                    SNS fan-out to: membership-realm-q, tenant-realm-q, billing-realm-q  (LocalStack, local dev only)
```

An outbox insert is atomic with the business write — a `2xx` HTTP response guarantees an `outbox_events` row exists.

### Step 1 — Start infrastructure

```bash
make docker-up
```

`scripts/init-localstack.sh` runs automatically (`SERVICES=sns,sqs,s3` in `docker-compose.yml`'s `localstack` block) and creates:

| Resource | Type |
|---|---|
| `iam-tenant-events` | SNS topic |
| `membership-realm-q` (+ `-dlq`) | SQS queue, filtered `EventType IN [DepartmentMembershipGranted, DepartmentMembershipLevelChanged, TenantRoleGranted, TenantRoleRevoked]` |
| `tenant-realm-q` (+ `-dlq`) | SQS queue, filtered `EventType IN [TrialStarted]` |
| `billing-realm-q` (+ `-dlq`) | SQS queue, filtered `EventType IN [TenantReactivated]` |

In production these three queues subscribe to **three different upstream topics** (O&M's `iam.membership.events`, O&M's `iam.tenant.events`, Billing's `billing.events`); in local dev, without those services running, the script subscribes all three to this service's own `iam-tenant-events` topic so a developer can hand-publish test events to any of them. Each SNS→SQS subscription sets `RawMessageDelivery=true`.

### Step 2 — Verify SNS/SQS exist

```bash
docker compose exec localstack awslocal sns list-topics --region ap-south-1
docker compose exec localstack awslocal sqs list-queues --region ap-south-1
```

### Step 3 — Point the server at the created resources

Copy the ARNs/URLs `init-localstack.sh` printed into `.env` (`SNS_TOPIC_TENANT_ARN`, `SQS_MEMBERSHIP_REALM_QUEUE_URL`, `SQS_TENANT_REALM_QUEUE_URL`, `SQS_BILLING_REALM_QUEUE_URL`), leave `GLUE_REGISTRY_TENANT_NAME` unset (falls back to `NoopCodec` — plain JSON, no Glue header), then:

```bash
make run
```

### Step 4 — Trigger an event

```bash
TENANT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
curl -s -X POST http://localhost:8080/api/v1/internal/tenants/provision-trial \
  -H "Content-Type: application/json" -H "Idempotency-Key: $(uuidgen)" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: iam-system" \
  -d "{\"tenant_id\":\"$TENANT_ID\",\"tenant_name\":\"Acme\",\"tenant_slug\":\"acme\",\"admin_email\":\"owner@acme.test\",\"admin_name\":\"Owner\",\"plan\":\"trial\"}" | jq .
```

A `201` means the row and the `TrialTenantProvisioned` outbox event committed atomically. Expect the message in `tenant-realm-q`-adjacent queues within ~500 ms (only in local dev, where all three queues subscribe to this one topic).

### Step 5 — Read messages from SQS

```bash
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/membership-realm-q \
  --max-number-of-messages 10 --region ap-south-1 | jq '.Messages[].Body | fromjson'
```

`receive-message` only hides messages for the visibility timeout — use `delete-message` with the `ReceiptHandle` to remove them permanently.

### Step 6 — Inspect the outbox in Postgres

```bash
docker compose exec postgres psql -U postgres -d provisioner -c \
  "SELECT id, event_type, attempts, published_at IS NOT NULL AS published, last_error
   FROM outbox_events ORDER BY created_at DESC LIMIT 20;"
```

### Troubleshooting events

| Symptom | Likely cause | Fix |
|---|---|---|
| Queue is empty after a successful write | SNS topic/subscription not created, or `.env`'s `SNS_TOPIC_TENANT_ARN` doesn't match | Re-check `docker compose logs localstack`; re-run Step 2 |
| `outbox_events` row has `last_error` containing `404 NotFound` | Topic ARN mismatch | Verify against `awslocal sns list-topics` output |
| `outbox_events` empty after a write | Row was published and pruned, or the write never committed | Re-check the HTTP response code — a `2xx` guarantees the row was committed |
| Messages keep reappearing after `receive-message` | Normal — visibility timeout, not deletion | Use `delete-message` |
| RP-6/RP-7 (IdP configure/remove) return `500` locally | OpenBao's Kubernetes-auth login has no real Kubernetes cluster to call out to in `docker-compose.yml` | Expected in local dev — seed OpenBao manually (`bao login <root token from docker compose logs openbao>`), or rely on `test/integration/openbao_test.go`, which exercises the real login flow with a stubbed TokenReview response |

---

## Testing

### Canonical tests (do not break)

- **`test/postgres/rls_test.go`** — the 5 canonical RLS cases (missing GUC → 0 rows; cross-tenant `INSERT`/`UPDATE` rejected; cross-tenant `SELECT`/`UPDATE` see/affect 0 rows; **Case 5** — no GUC leakage across a pooled PgBouncer backend, pool pinned to `MaxConns=1`) plus two structural tests (every tenant-scoped table has `rowsecurity=true AND forcerowsecurity=true`; `outbox_events`/`processed_events`/`exporter_watermarks` deliberately do not).
- **`internal/eventschema/asyncapi_drift_test.go`** — asserts `api/asyncapi.yaml`'s `iamTenantEvents` channel message list and `eventschema.ByEventType`'s map keys name exactly the same 9 events. This repo's CI-enforceable substitute for a live AWS Glue Schema Registry comparison.
- **RP-INV-1 / RLS-6 greps** — `.github/scripts/arch-lint.sh`: no `Nerzal/gocloak` import outside `internal/adapter/outbound/keycloak/`; no non-`SET LOCAL` write to `app.tenant_id`.

### Coverage

`.github/scripts/coverage-gate.sh` reads `go tool cover -func=coverage.out`'s total and fails if it's below `${COVERAGE_THRESHOLD:-90}` — its own default and stated ratchet target is **90%**. The currently checked-in `validate-test.yml` explicitly pins `COVERAGE_THRESHOLD: '80'` for the reusable workflow, so **80% is the threshold actually enforced by CI today**; the gate script's comment documents the intent to ratchet to 90% once every package is as thoroughly covered as the two below. Measured coverage: core service `internal/core/service/...` and HTTP handlers `internal/adapter/inbound/http/...` both at **~100%**, the Keycloak adapter at **~99.6%**, `cmd/reconciler/jobs` (once 0%) now at **~90.9%**, merged total coverage **~84.8%** — comfortably above the enforced floor, with real headroom still open before the 90% aspiration.

Coverage is measured over `./cmd/...`, `./internal/...`, `./pkg/...` (`COVER_PKG_LIST` in the Makefile), merged across the unit/postgres/integration suites via `scripts/merge_coverage.py` (max-count strategy).

---

## Environment variables

| Variable | Example | Default | Purpose |
|---|---|---|---|
| `APP_NAME` | `iam-realm-provisioner` | `iam-realm-provisioner` | Prometheus label, OTel service name |
| `APP_ENV` | `dev` | `dev` | Controls fail-fast dev-default checks and (with `DOCS_ENABLED`) the docs surface |
| `APP_PORT` | `8080` | `8080` | HTTP listen port |
| `METRICS_PORT` | `9090` | `9090` | Dedicated `/metrics` listener, never shared with the API |
| `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` | `localhost` / `5544` / `realm_provisioner_app` / — / `provisioner` / `disable` | — | App pool DSN parts (or set `DATABASE_URL` directly) — **required** unless `DATABASE_URL` is set |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` | `10` / `0` | `10` / `0` | Pool sizing |
| `PG_BOUNCER_MODE` | `false` | `false` | `true` in production — switches to transaction-scoped GUC injection |
| `PG_STATEMENT_TIMEOUT` | `5s` | `5s` | Per-statement timeout |
| `PG_SLOW_QUERY_THRESHOLD` | `200ms` | `200ms` | Log queries slower than this |
| `SYSTEM_DATABASE_URL` | `postgres://realm_provisioner_migrator:...@localhost:5544/provisioner?sslmode=disable` | — | **Required, even in dev.** BYPASSRLS pool for reconciler cross-tenant sweeps — must target `realm_provisioner_migrator` in production, never the app role |
| `MIGRATION_DATABASE_URL` | `postgres://realm_provisioner_migrator:...@localhost:5544/provisioner?sslmode=disable` | — | **Required, even outside `PG_BOUNCER_MODE`.** Direct (non-PgBouncer) DSN — only the migrator role can run `outbox.ApplySchema` / the domain migration |
| `AWS_REGION` | `ap-south-1` | `ap-south-1` | AWS region |
| `AWS_ENDPOINT_URL` | `http://localhost:4571` | — | LocalStack endpoint override (leave unset in production) |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `test` / `test` | — | LocalStack dev credentials (IRSA in production) |
| `SNS_TOPIC_TENANT_ARN` | `arn:aws:sns:...` | — | **Required outside dev.** `iam-tenant-events` topic ARN — events queue in the outbox but never publish without it |
| `GLUE_REGISTRY_TENANT_NAME` | `iam-tenant-events` | — | **Required outside dev.** Unset → `NoopCodec` (plain JSON) |
| `SQS_MEMBERSHIP_REALM_QUEUE_URL` / `SQS_TENANT_REALM_QUEUE_URL` / `SQS_BILLING_REALM_QUEUE_URL` | `https://sqs.../membership-realm-q` | — | The 3 inbound queues — an unset URL disables that one consumer with a `WARN`, not a startup failure |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_DRAIN_TIMEOUT` / `OUTBOX_RETENTION_DAYS` | `500ms` / `50` / `5` / `30s` / `8` | same | Outbox runner tunables |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | `8` | Consumer dedup retention (deliberately > the 7-day SQS message lifetime) |
| `KEYCLOAK_BASE_URL` | `http://localhost:8091` | — | **Required outside dev.** RP-INV-1's sole seam |
| `KEYCLOAK_ADMIN_REALM` | `master` | `master` | |
| `KEYCLOAK_ADMIN_CLIENT_ID` / `KEYCLOAK_ADMIN_CLIENT_SECRET` | `iam-realm-provisioner` / — | — | **`CLIENT_SECRET` required outside dev.** Keycloak admin service-account credential (OpenBao via ESO in prod, never env/image/git) |
| `KEYCLOAK_RATE_LIMIT_RPS` | `20` | `20` | Client-side token-bucket limiter; `Wait()` blocks rather than rejecting |
| `KEYCLOAK_TIMEOUT_MS` | `8s` (as written in `.env-example`) | `8000` | Per-operation timeout budget |
| `SHARED_TRIAL_REALM` | `trial` | `trial` | Shared trial realm name |
| `REALM_TEMPLATE_VERSION` | `v1` | `v1` | Active dedicated-realm template version |
| `BAO_ADDR` | `http://localhost:8210` | — | OpenBao address — Kubernetes auth (pod ServiceAccount token) |
| `BAO_AUTH_ROLE` / `BAO_KV_MOUNT` / `BAO_IDP_PATH_PREFIX` / `BAO_KEYCLOAK_ADMIN_PATH` | `iam-realm-provisioner` / `secret` / `iam/realm-provisioner/idp` / `iam/realm-provisioner/keycloak-admin` | same | OpenBao path/mount wiring |
| `ORG_MEMBERSHIP_BASE_URL` | `http://localhost:8082` | — | **Required outside dev.** O&M client — I-8 residual-MFA check |
| `REALM_EXPORT_BUCKET` / `REALM_EXPORT_KMS_KEY` | `iam-realm-exports` / — | `iam-realm-exports` / — | Offboarding export target |
| `TRIAL_GRACE_DAYS` / `CANCELLED_GRACE_DAYS` / `RETENTION_DAYS` | `30` / `30` / `90` | same | Lifecycle timers |
| `RECONCILER_BATCH_LIMIT` / `RECONCILER_TIMEOUT` / `RECONCILER_MAX_ATTEMPTS` | `100` / `5m` / `20` | same | Reconciler-job tunables — `RECONCILER_MAX_ATTEMPTS` is RP-C4's poison-pill cap |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | `true` / — | `true` / — | AsyncAPI viewer gating — always mounted outside production; opt-in via `DOCS_ENABLED` in prod, optionally bearer-gated |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | (unset) | OTLP/gRPC collector — provider is always installed; export is a no-op until this is set |

**Fail-fast:** `cmd/server/main.go`'s `validateRequiredEnv` panics at startup, listing **every** missing/misconfigured required var at once, if `KEYCLOAK_BASE_URL`, `KEYCLOAK_ADMIN_CLIENT_SECRET`, `ORG_MEMBERSHIP_BASE_URL`, `SNS_TOPIC_TENANT_ARN`, or `GLUE_REGISTRY_TENANT_NAME` is empty outside `APP_ENV ∈ {dev, development, local, test}` — plus a Postgres DSN check (`DATABASE_URL` or `PG_HOST`+`PG_USER`+`PG_PASSWORD`) and a `MIGRATION_DATABASE_URL` check when `PG_BOUNCER_MODE=true`.

---

## Security

| Topic | Guidance |
|---|---|
| **Keycloak admin credential ("the crown jewel")** | An OpenBao secret, surfaced via External Secrets Operator as an ephemeral volume — never an env var, image layer, or git artifact. RP-INV-1 means only this service's OpenBao role can `read` that path. |
| **Keycloak admin role set** | Master realm: `create-realm` only, not full `admin`. Managed realms: `realm-admin` composite (`manage-realm`/`manage-users`/`manage-clients`/`manage-identity-providers`). `create-realm` auto-confers admin on realms this account creates. |
| **Per-tenant IdP secrets** | OpenBao KV v2 at `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`, fully derivable from the `keycloak_idp_configs` row — no reference/pointer is ever stored in Postgres. RP-7 (remove IdP) deletes both the Keycloak IdP config and the OpenBao KV entry. |
| **Network isolation** | Every route under `/api/v1/internal/*` except `/healthz`/`/readyz`/`/asyncapi`/`/asyncapi.yaml` is mesh-only mTLS. `RequireSystemRole` is defense-in-depth — NetworkPolicy + mesh mTLS is primary. `/metrics` on its own dedicated port, monitoring-namespace-only. |
| **Input validation** | Fail-fast single-error (not collect-all); `strategy ∈ {shared, dedicated}`, `protocol ∈ {saml, oidc}`; `Idempotency-Key` non-empty ≤ 256 chars; IdP `config` validated by the §11.6 checklist **before** any Keycloak write; `setting_key` allow-listed. No secret material is ever accepted into or stored in Postgres. |
| **Authorization matrix** | RP-1..RP-10/12/15/16: internal system principal only (mesh peer). RP-11: internal system principal, incl. O&M/operator tooling. RP-13/RP-14: `platform_operator` only. RP makes no end-user authorization decision — that's AuthZ Enrichment's job. |
| **RLS** | `FORCE ROW LEVEL SECURITY` + `REVOKE ALL FROM PUBLIC` on the 5 tenant-scoped tables; only the `BYPASSRLS` role `realm_provisioner_migrator` can bypass, and the app role is actively stripped of `BYPASSRLS` if ever found set. |
| **§11.6 IdP region-portability checklist** | CRITICAL checks block with `400`; WARN checks require a recorded acknowledgement (admin `user_id` + timestamp) or `422`. Exists specifically so a customer-supplied IdP configuration can never pin the platform to a region ahead of a future cross-region migration. |
| **Cross-tenant admin access** | Requires `realm_provisioner_migrator` (`BYPASSRLS`), never the app role. A third role, `admin_readonly` (`NOLOGIN BYPASSRLS`), exists purely for compliance/support `SELECT`s and is never used by application code. |

---

## Observability

**SLOs.** This service has no user-facing request hot path — SLOs target operation success and freshness, not p99 latency. Availability target **99.9%** for the `/internal` provisioning API.

| Operation | Target |
|---|---|
| RP-1 trial provision | p95 ≤ 1.5 s |
| RP-2 direct-paid provision | p95 ≤ 4 s |
| RP-3 conversion | p95 ≤ 8 s + ~0.3 s/user migrated |
| RP-6 IdP configure | p95 ≤ 2 s |
| RP-8 realm-config apply | p95 ≤ 1 s |
| Trial-expiry / lapse freshness | ≤ one cron tick (daily) + retry |
| Realm-config reconcile freshness | ≤ one RP-C4 tick (10 min) |

### Metrics

All `iam_realm_provisioner_`-prefixed (`internal/adapter/outbound/metrics/metrics.go`, §25 frozen names): `provision_total{kind,result}`, `users_migrated_total`, `realm_active_gauge{strategy}`, `keycloak_call_duration_seconds{op}`, `keycloak_call_failures_total{op}`, `admin_token_refresh_total{trigger}` (`proactive`\|`401`), `idp_validation_total{outcome}`, `mfa_role_sync_total{action}`, `realm_config_pending_gauge`, `trial_expiry_processed_total`, `trial_expiry_deferred_total`, `lapse_processed_total{transition}`, `realm_export_total{result}`, `user_delete_total{result}`, `session_revoke_total{result}`, `mfa_reset_total{result}`, `template_drift{realm_version}`, `rls_violations_total{violation_type}` — plus passthrough `http_*` (`platform-gincommon`), `events_*`/`outbox_*`/`sqs_*` (`platform-events`, incl. `outbox_dead_letters_total`), and `pgcommon_*` pool/query instruments.

Three DB-state gauges (`realm_active_gauge`, `realm_config_pending_gauge`, `template_drift`) are refreshed every 5 minutes by exporter goroutines against the BYPASSRLS sys pool — a flat gauge plus a `gauge exporter query failed` log line means the exporter is broken, not that the count is stable. `trial_expiry_*`, `lapse_processed_total`, and `realm_export_total` are incremented correctly but only inside the one-shot reconciler CronJob process, which exits before a scrape lands — treat silence on those series as "no signal", not "no problem".

### Tracing and logs

`gincommon.InitTracingFromEnv()` in both binaries (always — in-process spans get valid trace IDs even without a collector; OTLP export is a no-op until `OTEL_EXPORTER_OTLP_ENDPOINT` is set), plus a `db.query` span per query. W3C `traceparent` is injected on every outbound HTTP call (O&M, OpenBao, Keycloak); SNS instead carries `trace_id` explicitly on the outbox envelope, so provision → event → downstream-projection stays one trace. Keycloak spans are named `keycloak.<op>`; a reconciler run is rooted at `reconciler.<job>`.

Structured log fields: `tenant_id`, `operation`, `idempotency_key`, `keycloak_realm`, `actor` — **never** the admin token, a password seed, or an IdP secret. Slow queries (> 200ms) log at WARN with `tenant_id` redacted; RLS violations log at ERROR, 1% sampled.

---

## Deployment

### Container image — two binaries

| Binary | Path in image | Purpose |
|---|---|---|
| `iam-realm-provisioner-server` | `/iam-realm-provisioner-server` | HTTP server (`cmd/server`) — the image's `ENTRYPOINT`. Serves `/api/v1/internal/*`, runs the outbox runner + 3 SQS consumers. |
| `iam-realm-provisioner-reconciler` | `/iam-realm-provisioner-reconciler` | One-shot reconciler (`cmd/reconciler`), dispatched via `--job=<name>` by the 6 K8s CronJobs. |

Two-stage `Dockerfile`: `golang:1.26.6-bookworm` builder, runtime is `gcr.io/distroless/static-debian12:nonroot` (no shell, non-root) — only the two compiled binaries and `deploy/keycloak/realm-template.json` are copied in, never the source tree (`api/asyncapi.yaml` is compiled in via `//go:embed`). `EXPOSE 8080 9090`.

### Helm chart

`deploy/helm/iam-realm-provisioner/` renders one `Deployment` (server) plus the 6 `CronJob`s above, all from the same image reference. HPA: `minReplicas: 2` / `maxReplicas: 4`, CPU 70% / memory 80%. PDB `minAvailable: 1`. Resources: CPU `250m`/`1`, Memory `256Mi`/`512Mi`. Pod AWS access (SNS/SQS/Glue/S3/KMS) is IRSA; secret access is OpenBao Kubernetes auth on the same ServiceAccount token.

### Migration safety

Rolling deploy, 2 replicas. Since nothing was deployed before LLD sign-off, the schema is one consolidated `000000_initial_schema` migration (`internal/adapter/outbound/postgres/migrations/`). `MIGRATION_DATABASE_URL` (direct, non-PgBouncer) runs `outbox.ApplySchema` **before** the domain migration — the domain migration `ALTER`s `outbox_events.payload`, which requires the table to already exist. Role/grant statements are wrapped in `DO $$ ... EXCEPTION WHEN insufficient_privilege ... END$$` blocks so the migration is safe in both dev (migrator lacks `CREATEROLE`) and production (Terraform owns `CREATE ROLE`).

---

## CI

Nine workflow files, including a tag-triggered `release.yml` (`VERSIONING.md` § Maintainer release process) and a schema-freeze watchdog:

- **`ci.yml`** — orchestrator. Runs `validate-test.yml` and `validate-quality.yml` in parallel with `build-image` (Hadolint → `.dockerignore` check → Buildx build cached to GHA/GHCR → Trivy CVE scan, CRITICAL/HIGH/UNKNOWN, `ignore-unfixed` → SARIF upload to the Security tab → smoke tests against **both** binaries). On `push` to `main`/`master` (needing `validate-test` to pass first): builds+pushes a `linux/amd64` image to GHCR with provenance+SBOM, signs it with Cosign keyless (Sigstore OIDC), and self-verifies the signature. Posts a `pr-summary` check on PR events; a `push-gate` job on push events requires `build-image` + `validate-quality`.
- **`release.yml`** — tag-triggered (`vMAJOR.MINOR.PATCH[-pre]`) release pipeline, modeled on sibling `iam-authz-enrichment`'s release workflow: `validate-test`/`validate-quality` re-run at the tagged commit → `build` (verifies the tag matches HEAD and the CHANGELOG has an entry, cross-compiles **both** binaries for 5 platforms) → `docker` (build/push the semver-tagged image, Trivy CVE scan, CycloneDX SBOM, SLSA provenance, Cosign sign+verify, then smoke tests against **both** binaries via the same `smoke-tests.sh` `ci.yml` uses) → `deploy-gate` (skipped unless `vars.DEPLOY_GATE_ENABLED == 'true'` — live Helm deploy, digest verification, `http_requests_total{status_class="5xx"}` health check, automatic rollback) → `publish` (GitHub Release with the CHANGELOG section as notes, binaries/checksums/SBOM/provenance attached).
- **`validate-test.yml`** (reusable) — `make test-ci` (unit + postgres/RLS + integration, `-race`, merged coverage) → `coverage-gate.sh` (`COVERAGE_THRESHOLD: '80'` pinned in this workflow) → uploads `coverage.out` as an artifact → `arch-lint.sh` (layering + RP-INV-1 + RLS-6) → `make test-e2e`.
- **`validate-quality.yml`** (reusable) — `go mod verify` → a grep rejecting HTML-escaped operators (`&gt;`/`&lt;`/`&amp;`) in workflow/script files → the RLS-6-style `check-forbidden-set-guc.sh` gate → `gofmt` check → `go mod tidy` drift check → `go vet` → `golangci-lint` → `govulncheck` → a Dockerfile digest-pin check (rejects a floating `FROM` tag without `@sha256:...`). The AsyncAPI-vs-Glue drift gate (`platform-schemagov validate`) runs separately, in `schema-registry.yml`'s `pr-check` job (see below).
- **`changelog-check.yml`** — fails a PR that touches `internal/`, `api/`, `deploy/`, `cmd/`, or `pkg/` without also updating `CHANGELOG.md`.
- **`schema-registry.yml`** — registers this service's 9 event schemas to the shared `iam-tenant-events` Glue registry: PR read-only validate+diff, push-to-`main`/release full validate→diff→register→changelog pipeline against staging/production. See `docs/runbook-schema-registry.md`.
- **`schema-prune.yml`** — monthly dry-run orphan report plus an operator `workflow_dispatch` execute path (archives to `docs/schema-archive/` before `glue:DeleteSchema`); hard-blocks execute while `iam-org-membership`'s `TenantCreated`/`TrialStarted` are present in the shared registry.
- **`schema-health-quarterly.yml`** — read-only quarterly GitHub Step Summary: lifecycle-annotation lint + Glue schema version-accumulation scan.
- **`freeze-watchdog.yml`** — daily cron alerting when `schema-registry.yml`'s `SCHEMA_FREEZE` environment variable (staging/production) has been left active too long (`::warning` at 24h, `::error`+job failure at 72h) — a forgotten freeze silently blocks all schema registration with no CI failure otherwise. Modeled on sibling `iam-user-profile`'s workflow of the same name.

`build-image`'s image reference is `ghcr.io/bcbp-solutions-fzc-llc/iam-realm-provisioner`.

**Required GitHub Actions repository secret: `GO_PRIVATE_TOKEN`.** Every workflow above that runs `go mod download` (`validate-test.yml`, `validate-quality.yml`, `ci.yml`'s `build-image`) needs this to fetch the private `platform-events`/`platform-gincommon`/`platform-pgcommon` modules — it's used to write `~/.git-credentials` before the download step. This is a **repository/organization Actions secret** (Settings → Secrets and variables → **Actions**), not the same store Dependabot uses (`dependabot.yml`'s own comment covers that separately) and not the local `$GO_PRIVATE_TOKEN` shell variable the `docker build --secret` example later in this doc (§ Docker) reads from your own environment. Without it, every CI job fails immediately at "Download modules" on the first push to a freshly created or forked repo. `CI_REPO_READ_TOKEN` (falls back to `github.token` if unset) and `release.yml`'s own secrets (`KUBECONFIG_B64`, documented in `VERSIONING.md`) are the other repository-level secrets this pipeline reads.

---

## Docker

### The published image ships two binaries

See the [Deployment](#deployment) section above — `/iam-realm-provisioner-server` (default entrypoint) and `/iam-realm-provisioner-reconciler` (invoked with `--job=<name>`).

### What the bundled `docker-compose.yml` starts

Unlike a compose file that starts infra-only, this repo's `docker-compose.yml` includes an `app` service too (built from the local `Dockerfile`, needs the `go_private_token` build secret) alongside its four infrastructure dependencies:

| Container | Image | Host port(s) | Purpose |
|---|---|---|---|
| `app` | built from local `Dockerfile` | `8080`, `9090` | This service itself, when run via `docker compose up` rather than `make run` |
| `postgres` | `postgres:16-alpine` | `5544 → 5432` | Primary store, seeded by `scripts/init-db.sql` |
| `localstack` | `localstack/localstack:4.4.0` | `4571 → 4566` | SNS/SQS/S3 (`SERVICES: sns,sqs,s3`) |
| `keycloak` | `quay.io/keycloak/keycloak:26.0` | `8091 → 8080` | `start-dev` mode, admin/admin — **not for production use** |
| `openbao` | `openbao/openbao:latest` | `8210 → 8200` | `server -dev` mode, in-memory, auto-unsealed — **not for production use**; prints its dev root token to `docker compose logs openbao` |

`make docker-up` only starts `postgres`, `localstack`, `keycloak`, and `openbao` — not `app` — so local dev typically still runs the service itself via `make run` for fast rebuilds.

### Building the service image

```bash
docker build -t iam-realm-provisioner:local --secret id=go_private_token,src=<(echo "$GO_PRIVATE_TOKEN") .
```

The `Dockerfile` requires the `go_private_token` build secret to fetch the three private `BCBP-SOLUTIONS-FZC-LLC` Go modules during the build; it errors out explicitly if the secret is missing or empty.

### Health and readiness

| Endpoint | Returns | Checks |
|---|---|---|
| `GET /healthz` | `200 {"status":"ok"}` | Pure liveness — never inspects a dependency |
| `GET /readyz` | `200 {"status":"ready","checks":{...}}` or `503 {"status":"not ready",...}` | Postgres, sys-Postgres, the outbox runner, and Keycloak's admin-token bootstrap (`Ready()`), each via its own `Health(ctx)` |

### Networking reference

| What you want to reach | Hostname inside Docker (compose network) | Hostname from the host |
|---|---|---|
| This service's API | `app` | `localhost:8080` |
| This service's `/metrics` | `app` | `localhost:9090` |
| PostgreSQL | `postgres:5432` | `localhost:5544` |
| LocalStack | `localstack:4566` | `localhost:4571` |
| Keycloak | `keycloak:8080` | `localhost:8091` |
| OpenBao | `openbao:8200` | `localhost:8210` |

### Calling the service from another container

```bash
# From inside another container on the same compose network
curl -X POST http://app:8080/api/v1/internal/tenants/provision-trial \
  -H "Content-Type: application/json" -H "Idempotency-Key: $(uuidgen)" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: <uuid>" -H "x-tenant-roles: iam-system" \
  -d '{"tenant_id":"<uuid>","tenant_name":"Acme","tenant_slug":"acme","admin_email":"owner@acme.test","admin_name":"Owner","plan":"trial"}'
```

### Integrating into another project's `docker-compose.yml`

```yaml
services:
  iam-realm-provisioner:
    image: ghcr.io/bcbp-solutions-fzc-llc/iam-realm-provisioner:latest
    ports:
      - "8080:8080"
    environment:
      APP_ENV: dev
      PG_HOST: iam-postgres
      PG_PORT: "5432"
      PG_USER: realm_provisioner_app
      PG_PASSWORD: devpassword
      PG_DBNAME: provisioner
      PG_SSLMODE: disable
      SYSTEM_DATABASE_URL: postgres://realm_provisioner_migrator:devpassword@iam-postgres:5432/provisioner?sslmode=disable
      MIGRATION_DATABASE_URL: postgres://realm_provisioner_migrator:devpassword@iam-postgres:5432/provisioner?sslmode=disable
      KEYCLOAK_BASE_URL: http://iam-keycloak:8080
      KEYCLOAK_ADMIN_CLIENT_ID: iam-realm-provisioner
      KEYCLOAK_ADMIN_CLIENT_SECRET: dev-secret-change-me
      ORG_MEMBERSHIP_BASE_URL: http://iam-org-membership:8080
      AWS_REGION: ap-south-1
      AWS_ENDPOINT_URL: http://iam-localstack:4566
      AWS_ACCESS_KEY_ID: test
      AWS_SECRET_ACCESS_KEY: test
      BAO_ADDR: http://iam-openbao:8200
    depends_on:
      iam-postgres:
        condition: service_healthy
```

The chart-level source for the image path is `deploy/helm/iam-realm-provisioner/values.yaml`'s `image.repository` (`ghcr.io/bcbp-solutions-fzc-llc/iam-realm-provisioner`); `VERSIONING.md` covers tag pinning (no `v*` release tags exist yet — pin `main`-branch images by digest, not `latest`, and verify the Cosign signature before deploying).

### Minimum required environment variables

```bash
# PostgreSQL
PG_HOST=localhost
PG_USER=realm_provisioner_app
PG_PASSWORD=<password>
PG_DBNAME=provisioner
SYSTEM_DATABASE_URL=postgres://realm_provisioner_migrator:...@localhost:5544/provisioner?sslmode=disable
MIGRATION_DATABASE_URL=postgres://realm_provisioner_migrator:...@localhost:5544/provisioner?sslmode=disable

# Keycloak (RP-INV-1)
KEYCLOAK_BASE_URL=http://localhost:8091
KEYCLOAK_ADMIN_CLIENT_SECRET=<secret>

# Org & Membership
ORG_MEMBERSHIP_BASE_URL=http://localhost:8082

# Events (outside dev)
SNS_TOPIC_TENANT_ARN=arn:aws:sns:ap-south-1:...:iam-tenant-events
GLUE_REGISTRY_TENANT_NAME=iam-tenant-events
```

Outside `APP_ENV ∈ {dev, development, local, test}`, `validateRequiredEnv` panics at startup listing every one of these that's missing.

---

## Cross-service dependencies

This service sits on **no synchronous request-path dependency for any other service's hot path** — every dependency below is about what happens to this service's own operations, never a dependency another service's user-facing request blocks on.

| Operation | Sync dependency | Posture | On failure |
|---|---|---|---|
| RP-1 / RP-2 provision | Keycloak Admin API | fail-closed | `502 keycloak_unavailable`; no local commit; retry with the same `Idempotency-Key` |
| RP-3 / RP-4 convert / revert | Keycloak Admin API | fail-closed | Compensating `DeleteRealmIfExists`; no local dedicated row |
| Invite (O&M → RP-5) | Keycloak Admin API | fail-closed | `502` → O&M `503`; no invitation |
| MFA reset (O&M → RP-9) | Keycloak Admin API | fail-closed | `502` → O&M `503`; no `MFAReset` — no reconciler |
| Realm-config (O&M → RP-8) | Keycloak Admin API | fail-open + durable reconcile | `pending` row, RP-C4 converges (RP-INV-6) |
| RP-6 / RP-7 IdP | OpenBao KV + Keycloak | fail-closed | No partial `keycloak_idp_configs` row |
| Offboarding export | S3 | fail-closed | Tenant stays `suspended`; `realm_export_total{result="fail"}` is critical |
| Outbox publish | SNS | retried, then DLQ | Local commit already happened — delay, not loss |
| RP-C3 suspend phase | O&M's I-16 (`SubscriptionLapseReader`) | fail-open, degrade to WARN | Skips the suspend phase only if `ORG_MEMBERSHIP_BASE_URL` is unset; offboard phase still runs |

Two fail-closed calls that sibling services feel (invite, MFA-reset) stop completing during a Keycloak outage; neither corrupts this service's local state.

**Open items:**
- **OpenBao Kubernetes auth is untested outside a real cluster** — in local dev, RP-6/RP-7's OpenBao write/delete step returns `500` as expected (no cluster to authenticate against); `test/integration/openbao_test.go` exercises the real login flow with a stubbed TokenReview response.

---

## Out of scope

| Concern | Where it lives |
|---|---|
| The organizational/business model — tenants-as-business-entities, memberships, department/role grants, plan/subscription state | Org & Membership |
| Credentials validation at request time or JWT issuance | Keycloak itself, at runtime |
| Delegation, tender ACL overlays, group→role mapping config | Their own sibling services |
| Any per-user table — there is none | Keycloak is the sole store of user identity/credentials |
| Secret material of any kind in Postgres | Realm passwords/seed credentials in Keycloak; IdP client secrets in OpenBao |
| End-user authorization decisions | AuthZ Enrichment |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, extending the service (new endpoint / event type / Keycloak operation / migration / env var), the two non-negotiable invariants (RP-INV-1, RLS-6), testing requirements, branch/commit conventions, and the PR checklist. See [VERSIONING.md](VERSIONING.md) for SemVer scope, the runtime-contract definition, image-tag scheme, and the maintainer release process.

| Document | Description |
|---|---|
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Top-level guidance for Claude Code working in this repo |
| [`.claude/architecture.md`](.claude/architecture.md) | Clean Architecture directory tree, shared library integration, dep rules |
| [`.claude/database-schema.md`](.claude/database-schema.md) | 9 tables (5 RLS-scoped + 3 exempt + 1 audit-only), enums, RLS invariants, triggers, migrations |
| [`.claude/api-caching-events.md`](.claude/api-caching-events.md) | Full endpoint catalogue (RP-1..RP-16), error taxonomy, event catalogue, AsyncAPI/drift mechanics |
| [`.claude/request-flows.md`](.claude/request-flows.md) | Keycloak-effect-first discipline, compensating teardown, every RP-*/RP-C* flow, idempotency mechanics, GDPR/data-lifecycle |
| [`.claude/operations.md`](.claude/operations.md) | Security, observability, configuration, CI/CD, testing, GDPR, integration points, open items |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Detailed architecture narrative with Mermaid diagrams — layer model, write flow, RLS/GUC flow, threat model |
| [`docs/lld/iam-lld-realm-provisioner-service.md`](docs/lld/iam-lld-realm-provisioner-service.md) | Full LLD, v1.3 Approved — §17 error taxonomy, §18.10 dependency table, §19 migration strategy, §25 frozen name inventory |
| [`docs/runbook.md`](docs/runbook.md) | Full operator procedures for the headline incident scenarios |

---

## License / ownership

BCBP Solutions FZC LLC — internal platform service. Not for external distribution.
