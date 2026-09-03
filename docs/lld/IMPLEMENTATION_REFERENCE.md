# Realm Provisioner — Implementation Reference (condensed from the frozen LLD v1.3)

Source: `docs/lld/iam-lld-realm-provisioner-service.md` (v1.3, Approved, 2026-09-02). This file is a
working extraction for implementation — where it and the LLD ever disagree, **the LLD (and its §25)
governs**; re-read the source section cited before finalizing anything security- or name-sensitive.

---

## 1. §25 Frozen Name Inventory (verbatim)

**Postgres — tables (7).** `tenant_realms`, `keycloak_idp_configs`, `keycloak_idp_config_validations`,
`realm_config_reconciliations`, `provisioning_operations` (five tenant-scoped, `FORCE` RLS on
`app.tenant_id`); `outbox_events` (schema owned by `platform-events` via `outbox.ApplySchema`),
`processed_events` (service DDL) — both RLS-exempt.

**Postgres — enum types + values (7).**

| Enum type | Frozen values |
|---|---|
| `realm_strategy` | `shared`, `dedicated` |
| `realm_status` | `provisioning`, `active`, `disabled`, `suspended`, `exported`, `deleted` |
| `idp_protocol` | `saml`, `oidc` |
| `idp_config_status` | `active`, `disabled`, `pending_validation` |
| `validation_outcome` | `pass`, `warnings_only`, `failing` |
| `recon_status` | `pending`, `applied`, `failed` |
| `op_status` | `in_progress`, `succeeded`, `failed` |

**String-valued discriminators (frozen, not DB enums).**
- `processed_events.consumer` ∈ {`membership`, `tenant`, `billing`} — one per inbound queue.
- `provisioning_operations.operation` ∈ {`provision_trial`, `provision_direct`, `convert`, `revert`, `mfa_reset`, …} (additive-only; also needs `create_invited_user`, `configure_idp`, `remove_idp`, `apply_realm_config`, `reactivate`, `expire_trial`, `suspend`, `delete_user`, `revoke_sessions` for RP-5/6/7/8/10/12/14/15/16 — LLD doesn't enumerate all, treat as additive per-endpoint operation name).
- `TenantSuspended.source` ∈ {`billing_lapse`, `operator`}.
- `TenantReactivated.source` ∈ {`billing`, `operator`}.

**Published events (9) on `iam-tenant-events`, frozen payload fields.**

| Event `type` | Frozen payload fields |
|---|---|
| `TrialTenantProvisioned` | `tenant_id`, `realm`, `keycloak_user_id` |
| `TenantRealmReady` | `tenant_id`, `realm`, `keycloak_shard`, `oidc_clients` |
| `TenantConverted` | `tenant_id`, `realm`, `plan`, `billing_subscription_id`, `users_migrated`, `direct_signup` |
| `DirectPaidSignup` | `tenant_id`, `realm`, `direct_signup` |
| `TrialExpired` | `tenant_id`, `trial_ends_at`, `grace_ends_at` |
| `TrialReactivated` | `tenant_id`, `new_trial_ends_at` |
| `TenantSuspended` | `tenant_id`, `realm`, `cancelled_at`, `suspended_at`, `source`, `reason` |
| `TenantOffboarded` | `tenant_id`, `realm`, `export_s3_key`, `offboarded_at` |
| `TenantReactivated` | `tenant_id`, `realm`, `source`, `reason` |

Envelope fields (frozen, shared with siblings): `id` (UUID v7), `type`, `source`
(`iam-realm-provisioner`), `specversion` (`"1"`), `time`, `data`, `tenant_id`, `trace_id`, optional
`subject`/`actor`/`dataschema`/`ip_address`/`user_agent`. SNS `MessageAttributes`: `EventType`,
`TenantID`, `Source`, `EventID`, `Subject`.

**Consumed events / queues (frozen).**
- `membership-realm-q` — filter `EventType IN [DepartmentMembershipGranted, DepartmentMembershipLevelChanged, TenantRoleGranted, TenantRoleRevoked]`
- `tenant-realm-q` — filter `EventType IN [TrialStarted]`
- `billing-realm-q` — filter `EventType IN [TenantReactivated]`
- Each has its own `-dlq` (e.g. `membership-realm-q-dlq`) and `maxReceiveCount=5`.

**Endpoint paths (frozen) — all under `/api/v1/internal`.**
```
RP-1  POST   /api/v1/internal/tenants/provision-trial
RP-2  POST   /api/v1/internal/tenants/provision-direct
RP-3  POST   /api/v1/internal/tenants/:id/convert
RP-4  POST   /api/v1/internal/tenants/:id/revert-conversion
RP-5  POST   /api/v1/internal/tenants/:id/users
RP-6  PUT    /api/v1/internal/tenants/:id/idp
RP-7  DELETE /api/v1/internal/tenants/:id/idp/:alias
RP-8  PATCH  /api/v1/internal/tenants/:id/realm-config
RP-9  POST   /api/v1/internal/tenants/:id/users/:uid/mfa-reset
RP-10 POST   /api/v1/internal/tenants/:id/reactivate
RP-11 GET    /api/v1/internal/tenants/:id/realm
RP-12 POST   /api/v1/internal/tenants/:id/expire-trial
RP-13 POST   /api/v1/internal/audit/idp-region-portability
RP-14 POST   /api/v1/internal/tenants/:id/suspend
RP-15 DELETE /api/v1/internal/tenants/:id/users/:keycloak_user_id
RP-16 POST   /api/v1/internal/tenants/:id/users/:keycloak_user_id/revoke-sessions
```
Unversioned infra/docs routes stay unversioned: `/healthz`, `/readyz`, `/metrics`, `/asyncapi`, `/asyncapi.yaml`.

**Metric prefix (frozen).** `iam_realm_provisioner_*`. Adding a new metric under the prefix is
additive; renaming an existing one is breaking.

**OpenBao secret path convention (frozen, RP-D10).**
`secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>` (KV v2) — deterministic, never
persisted as a reference in Postgres. Keycloak admin credential path (non-frozen-numbered but fixed
by config example, §12): `secret/data/iam/realm-provisioner/keycloak-admin` (`BAO_KEYCLOAK_ADMIN_PATH`).

**Glue registry (shared, two producers).** Registry `iam-tenant-events` (ap-south-1, JSON format).
This service registers 9 schema names (version 1 each): `TrialTenantProvisioned`, `TenantRealmReady`,
`TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`,
`TenantOffboarded`, `TenantReactivated`. O&M registers `TenantCreated`, `TrialStarted` in the same
registry — no name collision.

---

## 2. Full Data Model DDL (§4)

```sql
-- Enums (§4.1)
CREATE TYPE realm_strategy       AS ENUM ('shared', 'dedicated');
CREATE TYPE realm_status         AS ENUM ('provisioning', 'active', 'disabled', 'suspended', 'exported', 'deleted');
CREATE TYPE idp_protocol         AS ENUM ('saml', 'oidc');
CREATE TYPE idp_config_status    AS ENUM ('active', 'disabled', 'pending_validation');
CREATE TYPE validation_outcome   AS ENUM ('pass', 'warnings_only', 'failing');
CREATE TYPE recon_status         AS ENUM ('pending', 'applied', 'failed');
CREATE TYPE op_status            AS ENUM ('in_progress', 'succeeded', 'failed');

-- §4.2 tenant_realms
CREATE TABLE tenant_realms (
  tenant_id             uuid PRIMARY KEY,
  realm_name            text NOT NULL,
  strategy              realm_strategy NOT NULL,
  keycloak_shard        text,
  status                realm_status NOT NULL DEFAULT 'provisioning',
  realm_template_version text,
  trial_ends_at         timestamptz,
  converted_at          timestamptz,
  disabled_at           timestamptz,
  suspension_source     text,                                  -- 'billing_lapse'|'operator'|NULL (§8.8) — set by RP-14/RP-C3, cleared by RP-10
  exported_at           timestamptz,
  export_s3_key         text,
  record_version        bigint NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  deleted_at            timestamptz,
  CONSTRAINT chk_shard_dedicated  CHECK (strategy = 'shared' OR keycloak_shard IS NOT NULL),
  CONSTRAINT chk_shared_is_trial  CHECK (strategy = 'dedicated' OR realm_name = 'trial'),
  CONSTRAINT chk_suspension_source CHECK (suspension_source IS NULL OR suspension_source IN ('billing_lapse', 'operator'))
);
CREATE INDEX idx_tenant_realms_status   ON tenant_realms (status)         WHERE deleted_at IS NULL;
CREATE INDEX idx_tenant_realms_trial_exp ON tenant_realms (trial_ends_at)  WHERE strategy = 'shared' AND status = 'active';
CREATE INDEX idx_tenant_realms_strategy ON tenant_realms (strategy)       WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_tenant_realms_dedicated_name ON tenant_realms (realm_name) WHERE strategy = 'dedicated' AND deleted_at IS NULL;
CREATE INDEX idx_tenant_realms_lapse_offboard ON tenant_realms (disabled_at)
  WHERE status = 'suspended' AND suspension_source = 'billing_lapse';  -- RP-C3 offboard query, §8.8

-- §4.3 keycloak_idp_configs / keycloak_idp_config_validations
CREATE TABLE keycloak_idp_configs (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL,
  idp_alias      text NOT NULL,
  protocol       idp_protocol NOT NULL,
  display_name   text NOT NULL,
  enabled        boolean NOT NULL DEFAULT true,
  config         jsonb NOT NULL,
  status         idp_config_status NOT NULL DEFAULT 'pending_validation',
  record_version bigint NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT uq_idp_tenant_alias UNIQUE (tenant_id, idp_alias)
);

CREATE TABLE keycloak_idp_config_validations (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL,
  idp_alias         text NOT NULL,
  validator_version int  NOT NULL,
  outcome           validation_outcome NOT NULL,
  checks            jsonb NOT NULL,
  acknowledgements  jsonb NOT NULL DEFAULT '[]',
  validated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_idp_validations_tenant ON keycloak_idp_config_validations (tenant_id, idp_alias, validated_at DESC);

-- §4.4 realm_config_reconciliations / provisioning_operations
CREATE TABLE realm_config_reconciliations (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL,
  setting_key   text NOT NULL,
  desired_value jsonb NOT NULL,
  status        recon_status NOT NULL DEFAULT 'pending',
  attempts      int NOT NULL DEFAULT 0,
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  applied_at    timestamptz
);
CREATE INDEX idx_recon_pending ON realm_config_reconciliations (created_at) WHERE status = 'pending';

CREATE TABLE provisioning_operations (
  idempotency_key text PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  operation       text NOT NULL,
  status          op_status NOT NULL DEFAULT 'in_progress',
  result          jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  completed_at    timestamptz
);
CREATE INDEX idx_provops_tenant ON provisioning_operations (tenant_id, created_at DESC);

-- §4.5 outbox_events: DO NOT CREATE. platform-events' outbox.ApplySchema creates it at startup,
-- BEFORE this service's domain migration runs (§19). This service's migration only:
--   ALTER TABLE outbox_events ALTER COLUMN payload TYPE text;  -- + BEFORE INSERT normalize-payload trigger
--   GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events TO provisioner_app;  -- guarded by IF EXISTS
-- (copy the exact trigger/ALTER pattern from iam-org-membership's migration that does this)

-- processed_events: service-owned DDL
CREATE TABLE processed_events (
  event_id   text NOT NULL,
  consumer   text NOT NULL,       -- 'membership' | 'tenant' | 'billing'
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, consumer)
);
-- RLS-exempt (operational table), pruned past PROCESSED_EVENTS_TTL_DAYS (> 7-day SQS lifetime).
```

**RLS (§4.3, §10.1).** All five tenant-scoped tables (`tenant_realms`, `keycloak_idp_configs`,
`keycloak_idp_config_validations`, `realm_config_reconciliations`, `provisioning_operations`) carry
`FORCE ROW LEVEL SECURITY` with a fail-closed policy using `NULLIF` on `app.tenant_id`, e.g.:

```sql
ALTER TABLE tenant_realms FORCE ROW LEVEL SECURITY;
ALTER TABLE tenant_realms ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_realms
  USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
```
(Match the exact policy-naming/style already used in `iam-org-membership`'s migrations.)
`outbox_events`/`processed_events` are RLS-exempt. Roles: `provisioner_app` (no `BYPASSRLS`),
`provisioner_migrator` (has `BYPASSRLS`, runs migrations).

**No cross-database FKs** — `tenant_id` is a logical reference to Core `tenants(id)`, no local FK.

---

## 3. All 16 Endpoints (RP-1..RP-16) + health/docs

Every mutating route requires `Idempotency-Key` header (`400 missing_idempotency_key` if absent),
keyed into `provisioning_operations`. All under `/api/v1/internal`, mesh-only, mTLS, internal-route
guard (gincommon). `x-tenant-id` (+`x-user-id` for the system principal on RP→O&M calls, §18.3) comes
from gateway identity headers via `pkg/requestctx`, never the body.

### RP-1 `POST /tenants/provision-trial` — Signup Service
- Body: `{tenant_id, tenant_name, tenant_slug, admin_email, admin_name, hashed_password_seed, plan}`
- Flow (§8.1): create user in shared `trial` realm (email_verified=false, attr `tenant_id`, preserve `sub`) → trigger VERIFY_EMAIL → `RunInTx{ UPSERT tenant_realms(strategy='shared', realm_name='trial', status='active'), outbox TrialTenantProvisioned, provisioning_operations=succeeded }`.
- Response 201: `{tenant_id, realm:'trial', strategy:'shared', keycloak_user_id, email_verification_sent:true}`.
- Failure: Keycloak create fails → `502 keycloak_unavailable`, no local commit, retryable.
- Persists only `tenant_id` + realm row. No dedicated realm created (RP-INV-2). O&M's `tenants` row is created by Signup's own I-1 call, NOT by RP — RP's event only settles it.

### RP-2 `POST /tenants/provision-direct` — Signup Service
- Body: `{tenant_id, tenant_name, tenant_slug, admin_email, admin_name, plan, billing_subscription_id}` (no hashed_password_seed shown but likely also present per RP-1 parity — check consistency at impl time, not frozen in §25)
- Flow (§8.2): select least-loaded shard → clone realm-template → `tenant-<slug>` → register OIDC clients (tender-web, tender-mobile, tender-gateway, per-service) → create admin user, trigger VERIFY_EMAIL → on any KC failure: compensating teardown, `502 keycloak_unavailable` retryable → else `RunInTx{ UPSERT tenant_realms(strategy='dedicated', shard, status='active', converted_at=now()), outbox TenantRealmReady + TenantConverted(direct_signup=true), op=succeeded }`.
- Response 200: `{realm:'tenant-<slug>', strategy:'dedicated', oidc_clients[]}`.
- `payment_method_id` if sent is ignored (Billing/PCI domain).

### RP-3 `POST /tenants/:id/convert` — Billing Service
- Body: `{plan, billing_subscription_id, payment_method_id}` (`payment_method_id` ignored, never stored — PCI scope-out).
- Flow (§8.3): load `tenant_realms` (must be `strategy='shared'`) → select shard → clone template → dedicated realm (deterministic name from the **full** tenant UUID, not a truncated prefix — collision resistance matters once a realm-name conflict is treated as "my own retry," see below), OIDC clients (redirect scoped `/t/<slug>/*`) → loop each active trial user with matching `tenant_id` attr: create in dedicated realm preserving `sub`, copy email/name/attrs, copy TOTP credential; WebAuthn cannot migrate → flag `WEBAUTHN_REGISTER` required action → force-logout all shared-realm sessions for tenant → on partial failure: compensating teardown of the new realm **only while it has not yet received a migrated user** (once one user has been migrated the realm holds a user that no longer exists anywhere else — deleting it would be permanent data loss, not a rollback), `502 conversion_failed` retryable → else `RunInTx{ UPDATE tenant_realms(strategy='dedicated', shard, converted_at, status='active'), outbox TenantRealmReady + TenantConverted, op=succeeded }`. A retry with the same `Idempotency-Key` resumes rather than restarting: a `CreateRealmFromTemplate` name conflict on the deterministic realm name is tolerated as proof of the tenant's own earlier partial attempt, and a fresh per-tenant-user listing on the shared realm naturally picks up only whatever remains unmigrated.
- Response 200: `{tenant_id, realm, strategy:'dedicated', keycloak_shard, oidc_clients[], users_migrated, webauthn_reenroll_required, shared_sessions_terminated:true}`.
- Response field provenance: `tenant_id/realm/strategy/keycloak_shard` ← `tenant_realms` row; `oidc_clients` ← Keycloak state (not stored); `users_migrated/webauthn_reenroll_required/shared_sessions_terminated` ← per-op summary, mirrored into `provisioning_operations.result` for replay, carried on `TenantConverted` event — not a durable column.

### RP-4 `POST /tenants/:id/revert-conversion` — Billing / operator
- Reversible-within-7-days undo of conversion (§8.3 note): recreate shared-realm tenant (extended trial expiry), migrate users back, tear down dedicated realm — mirror of §8.3, same idempotency ledger.
- Beyond 7 days: `409 revert_window_expired` with `details.converted_at`; late reversion is an operator runbook (§24), not this endpoint.
- Body: `{reason?}` (optional, event/audit only).
- Resume-safe against a local-commit failure after a successful realm delete: if `DeleteRealmIfExists` succeeded but the subsequent local commit failed, a retry's user-listing call against the now-absent dedicated realm returns a distinguishable not-found signal (rather than a generic transport error) — treated as proof the migration-and-delete already completed, skipping straight to the local commit instead of hard-failing forever against a realm that no longer exists.

### RP-5 `POST /tenants/:id/users` — Org & Membership (`CreateInvitedUser`)
- Body: `{email, full_name, required_actions}` — O&M computes `required_actions` (`VERIFY_EMAIL`, `UPDATE_PASSWORD`, conditional `CONFIGURE_TOTP`); **RP applies verbatim, never re-derives** (F5).
- Creates invited user in tenant's realm (resolved via `tenant_realms`). No local persistence (no per-user table).
- Idempotency key required.

### RP-6 `PUT /tenants/:id/idp` — Org & Membership
- Body: `{idp_alias, protocol, config(non-secret), secret, ack[]}`.
- `idp_alias` must match `^[a-zA-Z0-9_-]{1,64}$` (checked before the idempotency ledger is even touched) — no dot permitted at all, since the OpenBao KV path is built by interpolating this value directly and a `.`/`..` sequence could otherwise resolve outside the tenant-scoped prefix.
- Ledgered against `Idempotency-Key` like every other mutating endpoint (§9.2): `idempotency.Begin` → replay a `succeeded` row's stored result, else proceed.
- Flow (§8.4): load `tenant_realms` (must be `strategy='dedicated'` — federation needs dedicated realm) else `409 idp_requires_dedicated_realm` → run §11.6 validator → CRITICAL failure: `INSERT keycloak_idp_config_validations(outcome='failing')`, `400 idp_region_portability_failed {details.failed_checks[], details.validator_version}` → WARN(s) unacknowledged: `INSERT validations(outcome='warnings_only')`, `422 idp_warnings_unacknowledged {details.warnings[]}` → pass or WARN-acked: write secret to OpenBao KV (`secret/data/iam/realm-provisioner/idp/<tenant_id>/<alias>`) → create/update IdP + attribute mappers on Keycloak — on failure here, if the secret was just written **and** this alias had no prior config (confirmed via a lookup before the write), best-effort delete the now-orphaned OpenBao secret; an update to an already-configured alias never deletes on failure, since the pre-existing Keycloak IdP is unaffected by the failed update → `RunInTx{ UPSERT keycloak_idp_configs(status='active', NO secret column), INSERT validations(outcome), op=succeeded }`.
- Response 200: `{idp_alias, status:'active'}`. Emits audit event `TenantIdpConfigChanged` (Audit only — NOT one of the 9 tenant-lifecycle events, no `iam.tenant.events` publish for IdP changes).

### RP-7 `DELETE /tenants/:id/idp/:alias` — Org & Membership
- Idempotent (not idempotency-key-guarded per catalogue "yes" not "key" — treat as safe-to-repeat delete). Deletes Keycloak IdP + OpenBao KV secret + `keycloak_idp_configs` row.

### RP-8 `PATCH /tenants/:id/realm-config` — Org & Membership (`PatchRealmConfig`)
- Body: `{setting_key, desired_value}`. RP-INV-6: synchronous, never evented.
- Flow (§8.10): apply setting to Keycloak realm (e.g. `local_accounts_enabled`) → success: `RunInTx{ record applied, op=succeeded }`, `200 {applied:true}` → Keycloak transient failure: `INSERT realm_config_reconciliations(status='pending', desired_value)`, `202 {applied:false, reconciling:true}` (RP-C4 retries on `*/10 * * * *`).
- `setting_key` must be allow-listed; else `400 unsupported_realm_setting {details.allowed[]}`.

### RP-9 `POST /tenants/:id/users/:uid/mfa-reset` — Org & Membership (`ResetMFA`, via O&M P-34)
- Flow (§8.6): idempotency/ledger check → resolve realm from `tenant_realms` → GET user's OTP+WebAuthn credentials → if any: DELETE each credential (per-credential, delete-if-present) → add `CONFIGURE_TOTP` required action ONLY if user still holds `requires-mfa` realm role → `204`. If none enrolled: `204` (idempotent no-op).
- **Fail-closed**: Keycloak 5xx/timeout → `502 keycloak_unavailable`, never silent success, no reconciler (unlike RP-16). O&M maps this to its own `503 realm_provisioner_unavailable` and does NOT emit `MFAReset` audit.
- Persists nothing locally (no per-user table). Does NOT terminate live sessions (separate RP-16 call needed for that).
- Metric: `iam_realm_provisioner_mfa_reset_total{result}`.

### RP-10 `POST /tenants/:id/reactivate` — Billing / operator / reconciler
- Two shapes (§8.11): **trial** reactivation (within 30-day grace) — re-enable disabled trial user, emit `TrialReactivated{tenant_id, new_trial_ends_at}`. **paid** reactivation — re-enable dedicated realm if it was `suspended` (whether from billing lapse OR operator RP-14 suspend — both are reversible via this one endpoint), emit `TenantReactivated{tenant_id, realm, source, reason}` with `source=operator` (this is the RP-produced side; billing's own reactivation of `source=billing` arrives via the `billing-realm-q` consumer, not this endpoint).
- Rejected if `offboarded` (terminal, RP-INV-5).
- Idempotent: reactivating an already-active tenant → `200` no-op, replay stored result.

### RP-11 `GET /tenants/:id/realm` — O&M / operator (read-only)
- Reads `tenant_realms` row: `{tenant_id, realm_name, strategy, keycloak_shard, status, ...}`. No side effects.

### RP-12 `POST /tenants/:id/expire-trial` — reconciler (RP-C1) only
- Idempotent per-tenant trial-expiry effect: mark `tenant_realms.disabled_at`, disable the tenant's shared-realm users, emit `TrialExpired{tenant_id, trial_ends_at, grace_ends_at}`. The RP-C1 cron calls this once per expired row (fan-out from cron to per-tenant endpoint call, not inline cron logic — keeps the effect idempotent+replayable via the same ledger).

### RP-13 `POST /audit/idp-region-portability` — operator only (read-only, whole-estate scan)
- Runs §11.6 checklist across every tenant realm's IdP configs; returns `{total_tenants, total_idp_configs, passing, warnings_only, failing, failing_tenants[]}` (HLD §11.6 verbatim shape).

### RP-14 `POST /tenants/:id/suspend` — operator only (RP-D12, §8.12)
- Body: `{reason}`. Requires platform-admin role (§10.4).
- Flow: if `offboarded` → `409 tenant_offboarded`; if already `suspended` → `200` idempotent no-op replay; if dedicated realm → disable Keycloak realm `tenant-<slug>` → `RunInTx{ UPDATE tenant_realms(status='suspended', disabled_at=now()), outbox TenantSuspended(source='operator', reason), op=succeeded }`, `200 {status:'suspended'}`; if shared-trial tenant → disable ONLY that tenant's users in shared `trial` realm (NEVER disable the shared realm itself) → same outbox/response pattern.
- Effect-only — does not set `subscription_status` (O&M's job). Reversible via RP-10.

### RP-15 `DELETE /tenants/:id/users/:keycloak_user_id` — Org & Membership (`DeleteUser`, F2)
- Idempotent: delete-if-exists → `204` always (never `404` for absent user — must converge cleanly for O&M's PI-9 reconciler retries). Signature match: `DeleteUser(ctx, tenantID, keycloakUserID)`.
- Realm resolved from `tenant_realms` (shared or dedicated). No local persistence.
- Only error: `keycloak_unavailable` (`502`, retryable).

### RP-16 `POST /tenants/:id/users/:keycloak_user_id/revoke-sessions` — Org & Membership (`RevokeUserSessions`, F3)
- Idempotent: logout + invalidate access/refresh tokens for tenant realm → `204` even with no active sessions.
- O&M treats this **best-effort/fail-open**: a `5xx`/timeout does NOT block or reverse O&M's own state change; O&M relies on its own TTL backstop, not an RP reconciler. RP returns fast, no durable retry needed on RP's side.
- Note: RP-15's hard-delete already terminates sessions at the identity layer — O&M does not call RP-16 additionally on the delete path.

### Health/docs (unversioned)
- `GET /healthz` — liveness.
- `GET /readyz` — DB reachability + Keycloak-admin-token bootstrap + Glue schema-version resolution.
- `GET /metrics` — dedicated listener on `METRICS_PORT` (default 9090).
- `GET /asyncapi`, `GET /asyncapi.yaml` — HTML viewer + raw spec, `//go:embed`, gated by `DocsConfig` (§12.1).

---

## 4. Events — 9 produced + 4 consumed

### 4.1 Produced (on `iam-tenant-events`, via outbox) — §7.4/§7.5

| Event | Emitted when | Payload | Fan-out queues |
|---|---|---|---|
| `TrialTenantProvisioned` | RP-1 success | `tenant_id, realm('trial'), keycloak_user_id` | `tenant-orgm-q`, `tenant-audit-q` |
| `TenantRealmReady` | RP-2 or RP-3 success (never for trial) | `tenant_id, realm, keycloak_shard, oidc_clients[]` | `tenant-orgm-q`, `tenant-audit-q` |
| `TenantConverted` | RP-3 (or RP-2 with `direct_signup=true`) | `tenant_id, realm, plan, billing_subscription_id, users_migrated, direct_signup` | `tenant-orgm-q`, `tenant-audit-q`, `tenant-notif-q`, `tenant-authz-q` |
| `DirectPaidSignup` | RP-2 completion | `tenant_id, realm, direct_signup` | `tenant-orgm-q`, `tenant-audit-q`, `tenant-notif-q` |
| `TrialExpired` | RP-C1/RP-12 | `tenant_id, trial_ends_at, grace_ends_at` | `tenant-orgm-q`, `tenant-audit-q`, `tenant-notif-q` |
| `TrialReactivated` | RP-10 (trial branch) | `tenant_id, new_trial_ends_at` | `tenant-orgm-q`, `tenant-audit-q` |
| `TenantSuspended` | RP-C3 lapse sweep (`source=billing_lapse`) or RP-14 (`source=operator`) | `tenant_id, realm, cancelled_at, suspended_at, source, reason` | `tenant-orgm-q`, `tenant-audit-q`, `tenant-notif-q` |
| `TenantOffboarded` | RP-C3 after confirmed S3 export + realm delete | `tenant_id, realm, export_s3_key, offboarded_at` | `tenant-orgm-q`, `tenant-audit-q` |
| `TenantReactivated` | RP-10 (paid branch, `source=operator`) | `tenant_id, realm, source, reason` | `tenant-orgm-q`, `tenant-audit-q`, `tenant-notif-q` |

Cron-origin events (`TrialExpired`, `TenantSuspended`, `TenantOffboarded`) carry system sentinels:
`ip_address:"system"`, `user_agent:"iam-realm-provisioner/<job-name>-cron"`, `actor: SystemActorID`.

`TenantReactivated` is both produced (here, `source=operator`, topic `iam.tenant.events`) and consumed
(Billing's own, `source=billing`, topic `billing.events` → `billing-realm-q`) — no collision, disambiguated by `source` and by topic (RP-D13).

Glue: registry `iam-tenant-events`, JSON format, all 9 schemas version 1. Enqueue-time: plain JSON
validated against embedded JSON Schema (`internal/eventschema/`), no Glue codec yet. Publish-time
(outbox.Runner): `events.NewSNSPublisher` wraps with `events.WithCodec(glueCodec)` which resolves +
stamps `dataschema` (Glue schema-version UUID). Dev/test uses `NoopCodec` (no Glue header, no `dataschema`).

```go
// Enqueue pattern (service layer, inside state-change tx):
return pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
    if err := realmRepo.UpsertDedicated(ctx, tx, tenantID, realm); err != nil { return err }
    payload, _ := json.Marshal(tenantConvertedPayload)
    if err := validator.Validate(ctx, "TenantConverted", payload); err != nil { return err }
    env := events.NewEnvelope("TenantConverted", "iam-realm-provisioner", json.RawMessage(payload),
        events.WithTenantID(tenantID), events.WithTraceID(rc.TraceID), events.WithActor(rc.CallerID),
        events.WithSchemaVersion("1"))
    return outbox.Enqueue(ctx, tx, env)
})
// Publish (cmd/server outbox.Runner):
publisher := events.NewSNSPublisher(snsConfig, events.WithCodec(glueCodec)) // registry "iam-tenant-events"
```

Schema evolution: additive (new optional field/enum value) → new Glue version, bump `dataschema`,
update `api/asyncapi.yaml` same PR. Breaking → new Glue schema name + new AsyncAPI message + major bump.

### 4.2 Consumed (§7.1)

| Queue | DLQ | maxReceiveCount | Filter | Dispatched to |
|---|---|---|---|---|
| `membership-realm-q` | `membership-realm-q-dlq` | 5 | `EventType IN [DepartmentMembershipGranted, DepartmentMembershipLevelChanged, TenantRoleGranted, TenantRoleRevoked]` | `MfaService.SyncRequiresMfa` |
| `tenant-realm-q` | `tenant-realm-q-dlq` | 5 | `EventType IN [TrialStarted]` | `LifecycleService.RecordTrialTimer` (upsert `tenant_realms.trial_ends_at`) |
| `billing-realm-q` | `billing-realm-q-dlq` | 5 | `EventType IN [TenantReactivated]` | `LifecycleService.ReactivateFromBilling` |

Each dedups via `processed_events` `INSERT ... ON CONFLICT DO NOTHING` keyed `(event_id, consumer)`,
`consumer` = `membership`/`tenant`/`billing` respectively. Both O&M and Billing Glue-encode, so both
consumers need a decode-capable codec (`events.WithConsumerCodec`) regardless of this service's own
publish-side config — the Glue wire header is self-describing.

`tenant`/`billing` write to Postgres, so the mark commits **inside** the same `TxRunner.RunInTx` as the
projection update (`MarkProcessed` joins the ambient tx) — neither lands without the other.
`billing`'s Keycloak `EnableRealm` stays outside that tx (Keycloak-effect-first, §9.1). `membership`
touches no local state, so it marks after success; its effect is a reconcile, making replay a no-op.

**`membership-realm-q` semantics (§8.6, RP-EVT-6).**
- Add `requires-mfa` **unconditionally** (assign-if-absent) on: `DepartmentMembershipGranted` at reviewer/approver level, `DepartmentMembershipLevelChanged` elevating to reviewer/approver, `TenantRoleGranted`.
- Drop `requires-mfa` **only** on `TenantRoleRevoked`, and only after calling O&M's I-8
  `GET /api/v1/internal/users/:id/memberships` to check for residual approver/reviewer level or any
  other MFA-requiring role — revoke-if-present only if NONE remain. `DepartmentMembershipRevoked` is
  NOT routed here (not in the filter) — do not build drop logic for it.
- Written as a **reconcile** (recompute desired state, assign-if-absent/revoke-if-present), not
  apply-a-delta — so redelivery/out-of-order convergence is automatic.

**`tenant-realm-q` semantics (§7.1, RP-4).** Idempotent upsert of `trial_ends_at` onto the existing
`tenant_realms` row (row already exists — created synchronously at RP-1 before O&M's `TrialStarted`
would even fire). RP never queries O&M for trial data — this is push-only from O&M.

**`billing-realm-q` semantics (§7.1, §8.11, RP-EVT-7).** If tenant's realm was `suspended` → re-enable
dedicated realm. If tenant was only `cancelled`/read-only (never reached `suspended`) → no Keycloak
effect, just record the processed event (no-op).

A message whose payload fails schema-decode routes to its DLQ rather than retrying indefinitely.

---

## 5. Reconciler jobs (`cmd/reconciler --job=<name>`)

| Job | ID | Schedule | What it does |
|---|---|---|---|
| `trial_expiry` | RP-C1 | `0 2 * * *` | Sweep own `tenant_realms` for `strategy='shared' AND status='active' AND trial_ends_at < now()` (uses `idx_tenant_realms_trial_exp`) — **never queries O&M**. For each: call RP-12 (`expire-trial`) per tenant so the effect goes through the same idempotency ledger. Batched (`batchLimit=100` per §12 config), self-retrying. |
| `trial_hard_delete` | RP-C2 | `0 3 * * *` | For trials past `TRIAL_GRACE_DAYS` (30) grace: hard-delete users from shared realm via Admin API, soft-delete/scrub local `tenant_realms` row (retain only `tenant_id` for audit-FK, preserve `sub`). O&M runs its own separate cleanup cron for its `tenants` row — no coordination needed. |
| `subscription_lapse` | RP-C3 | `0 2 * * *` | **Two phases sharing one `BatchLimit` budget** (`cmd/reconciler/jobs/subscription_lapse.go`). **Suspend** (skipped, WARN-logged, if `ORG_MEMBERSHIP_BASE_URL` unset): `port.SubscriptionLapseReader.ListLapsedSubscriptions` calls O&M's real **I-16** `GET /api/v1/internal/subscription-lapses` (`x-user-id`+`x-tenant-roles=iam-system`, no `x-tenant-id`) for tenants already cancelled past O&M's grace math; skips non-`dedicated` `realm_type` (owned by RP-C1/RP-C2 instead); per tenant, no-op if already `suspended`/`deleted`, else disable dedicated realm → `status='suspended'`, `disabled_at=now()`, `suspension_source='billing_lapse'`, emit `TenantSuspended(source='billing_lapse')`, key `lapse-suspend:<tenant_id>`. **Offboard** (always runs, purely local — no O&M call): `TenantRealmRepository.ListSuspendedPastRetention(ctx, RETENTION_DAYS, limit)` selects `status='suspended' AND suspension_source='billing_lapse' AND disabled_at < now()-retentionDays` via `idx_tenant_realms_lapse_offboard`; per candidate, export realm to S3 (encrypted, confirm durable write) THEN delete realm, `status='deleted'`, scrub row, emit `TenantOffboarded` (terminal, RP-INV-5), key `offboard:<tenant_id>`. **Excludes** operator-sourced suspensions (`suspension_source='operator'`) from the offboard leg **structurally** — the query's own `suspension_source='billing_lapse'` predicate, not a runtime check — so they stay `suspended` indefinitely until an operator explicitly reactivates/offboards. Never deletes without confirmed export (RP-EVT-4) — on export/S3 failure, leaves `suspended`, retries next tick. |
| `realm_config_reconcile` | RP-C4 | `*/10 * * * *` | Retries every `realm_config_reconciliations` row with `status='pending'`: re-attempt the Keycloak apply; on success mark `applied`, `applied_at=now()`; on failure increment `attempts`, record `last_error`, and leave the row `pending` for the next tick UNLESS the just-incremented attempt count has reached `RECONCILER_MAX_ATTEMPTS` (default 20), in which case the row moves atomically to a terminal `failed` status instead — a poison-pill cap so a permanently-broken setting stops consuming a batch slot every tick forever. |
| `outbox_prune` | — | (own ticker, not cron) | Calls `platform-events` `outbox.Runner.PrunePublished` on a retention ticker — runs as a `cmd/server` background goroutine per §3.1, not necessarily a separate reconciler CronJob (verify against sibling pattern — org-membership likely wires this in `cmd/server`, not `cmd/reconciler`). |
| `processed_events_prune` | — | — | Prune `processed_events` rows older than `PROCESSED_EVENTS_TTL_DAYS`. |

All reconciler jobs: idempotent per tenant (safe to re-run), batched, must **increment**
`*_deferred_total` metrics on every defer (not just register them — §11.2 instrumentation requirement,
CI/alerting depends on this actually incrementing).

---

## 6. §9 Concurrency, Consistency, Failure Handling

**Two ordering disciplines per flow (§9.1):**
1. **Keycloak-effect-first, then local commit** — used for provisioning/migration/IdP/MFA/realm-config-apply-success. KC mutation runs first; only after success does `RunInTx` commit the local row + outbox event atomically. Crash after KC effect but before local commit → recovered by idempotent replay (same `Idempotency-Key`; KC step becomes a no-op reconcile since it already exists; local commit then lands). A committed local row ALWAYS reflects a real KC state.
2. **Local-intent-first, then reconcile** — used ONLY for realm-config when Keycloak is transiently down (§8.10). Desired value recorded `pending`, RP-C4 applies later.

**Idempotency ledger (`provisioning_operations`, §9.2).** Every externally-triggered mutation
(RP-1..RP-10, RP-12, RP-14, and the 3 SQS handlers) is keyed here. On entry: upsert
`(idempotency_key, in_progress)`. Key already `succeeded` → replay stored `result`, no re-execution.
Key `in_progress` (concurrent duplicate) → `409 operation_in_progress` (retryable). Keys are
caller-supplied via `Idempotency-Key` header (HTTP callers) or **derived from the envelope `id`** for
SQS-driven handlers.

**Compensation (§9.3).** Multi-step KC sequences (realm create + OIDC clients + user migration) that
fail partway get a **compensating teardown** deleting the partially-created dedicated realm, returning
a retryable error, leaving pre-existing state authoritative. Local commit is always the LAST step, so
a compensated failure leaves no local dedicated row — retry starts clean. Teardown itself is
idempotent (delete-if-exists).

**Keycloak resilience (§9.4).** `keycloak` adapter wraps gocloak with: shared admin-token cache +
single-flight refresh; bounded exponential-backoff retry on 5xx/timeout/connection errors (429 honours
`Retry-After`); client-side rate limiter (bursts during mass migration); per-operation timeout budget.
Errors surface as typed domain errors: `keycloak_unavailable`, `realm_already_exists`,
`user_already_exists` — so flows can tell retryable transport failures from semantic conflicts
(often idempotent no-ops).

**Shard placement (§9.5).** At conversion/direct-paid: place on least-loaded Keycloak shard, record
`keycloak_shard`. Reads current per-shard realm counts from the LOCAL mirror (this service is single
writer → complete count). Tie breaks deterministically by shard id. Placement fixed for tenant's life.

**Concurrency control (§9.6).** Optimistic locking via `record_version` on `tenant_realms`/
`keycloak_idp_configs` — concurrent second mutation loses with `409 optimistic_lock_conflict`
(`details.record_version`). The idempotency ledger's `in_progress` state additionally serializes
concurrent duplicates of the SAME operation. Distinct tenants are fully parallel (RLS isolates them).

**RLS/GUC bridge rule (§10.1, RLS-6).** ALWAYS `SET LOCAL app.tenant_id` via
`pgcommon.GUCSetFromContext`, transaction-scoped — **never** a session-level `SET`. CI greps for the
forbidden non-`LOCAL` form.

**Failure taxonomy (§9.7):**

| Failure | Detection | Handling |
|---|---|---|
| Keycloak transient (5xx/timeout) | typed `keycloak_unavailable` | retry w/ backoff; no local commit; caller retries same key |
| Keycloak semantic conflict | typed conflict error | idempotent success if desired end-state matches; else surfaced |
| Partial provisioning crash | local row absent on retry | compensating teardown + clean retry |
| Realm-config KC-down | apply step fails | record `pending`, RP-C4 reconciles |
| DB down | `RunInTx` error | operation fails retryably; nothing published (outbox in same tx) |
| SNS publish failure | `outbox.Runner` attempts | retried; DLQ after `OUTBOX_MAX_ATTEMPTS`; `outbox_dead_letters_total` |
| Duplicate event redelivery | `processed_events` conflict | no-op |
| Export-before-delete failure | S3 write not confirmed | left `suspended`, retried; never deleted without confirmed export |

---

## 7. §10 Security

**Keycloak admin role set (§10.5, RP-2 resolved):**
- Master realm: `create-realm` **only** (not full master `admin`). Required for `POST /admin/realms`.
- Managed realms: `realm-admin` composite (covers `manage-realm`/`manage-users`/`manage-clients`/`manage-identity-providers`) — covers realm enable/disable, client registration, user CRUD (RP-15), IdP config (RP-6), realm-role assign/revoke (RP-1), credential reset (RP-9), session/token revocation (RP-16).
- `create-realm` auto-confers admin on realms this account creates — no per-realm bootstrap hook needed since RP is sole creator (RP-INV-1).
- Cross-realm migration (trial→dedicated) — ONE account holds rights on both source (`view-users`/`query-users` via `realm-admin` on `trial`) and target (`manage-users`/`manage-realm` via `realm-admin` on new realm).
- Deferred hardening (not MVP): split bootstrap (`create-realm` only) vs runtime (`realm-admin`) accounts.

**Credential protection (§10.5 — "the crown jewel").** Keycloak admin credential: OpenBao secret,
surfaced via ESO (OpenBao/Vault provider) as an ephemeral volume — NEVER env var, image layer, or git
artifact. RP-INV-1 means only this service's OpenBao role can `read` that path. Admin access token:
cached only in Valkey/in-process (§6.1), never logged, never in Postgres, shortest practical lifetime.

**Per-tenant IdP secrets (§10.5, §4.2, RP-D10).** Dynamic, unbounded, so accessed via OpenBao KV API
at runtime (not ESO). Path: `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`. No
reference/pointer ever stored in Postgres — path is fully derivable from `(tenant_id, idp_alias)`
already on the row. OpenBao policy (Kubernetes auth, pod ServiceAccount token → OpenBao role → policy):
grants `create`/`read`/`update`/`delete` on `secret/data/iam/realm-provisioner/idp/*` + `read` on the
Keycloak-admin path ONLY — nothing else. IdP secret passes through the HTTP handler transiently on the
RP-6 request, straight to OpenBao + Keycloak — never persisted or echoed. Keycloak also keeps its own
encrypted copy (for federated-login-time use); OpenBao lets RP re-apply/rotate without customer
re-submission. RP-7 removal deletes ALL copies (Keycloak IdP + OpenBao KV entry).

**Network isolation (§10.2).** All routes `/api/v1/internal/*` except health/docs — mesh-only, mTLS,
internal-route guard. `/metrics` on its own listener, monitoring-namespace-only.

**Input validation (§10.3).** `strategy`/`protocol` enum-checked; slug charset/length for realm
naming; `Idempotency-Key` non-empty ≤256 chars; IdP `config` validated by §11.6 BEFORE any Keycloak
write (RP-INV-4); `setting_key` allow-listed (else `400 unsupported_realm_setting`). No secret material
ever accepted into or stored in Postgres.

**Audit (§10.6).** Every mutation emits an event or `TenantIdpConfigChanged` audit event, carrying
`actor`, `trace_id`, system sentinel for cron-origin ops.

**Authorization (§10.4).** RP makes NO end-user authz decision (that's AuthZ Enrichment). Callers are
trusted mesh peers (internal-route guard + mTLS); `x-tenant-id` header scopes + RLS enforces. RP-13
(operator-only) additionally requires operator role in `x-tenant-roles`/platform-admin claim. RP-14
also requires platform-admin role.

**§11.6 Region-Agnostic IdP Configuration Checklist (§8.5):**
- **CRITICAL (block, `400`):** (1-2) SAML SSO/SLO URL hostnames carry no AWS-region substring; (3)
  customer-provided signing cert; (4-6) ACS/SLO/EntityID point at region-neutral
  `auth.bcbpsolutions.com/realms/<tenant>/...`; (10) OIDC issuer/discovery region-neutral; (11) OIDC
  redirect URIs region-neutral; (12) OIDC audience matches region-neutral client ID.
- **WARN (allow w/ recorded ack, `200`):** (7) SAML NameID format `transient`; (8) custom
  attribute-mapper URN region substrings; (9) SAML signature algorithm ≥ RSA-SHA256; (13) OIDC PKCE
  `S256`. Ack recorded with admin's `user_id` + timestamp in `keycloak_idp_config_validations.acknowledgements`.
- Check IDs referenced elsewhere as `check_1_sso_url_region`, `check_4_acs_url_region`, etc.
- Portability audit (RP-13, §8.5.3): operator-only, runs full checklist against every tenant's live
  IdP configs; report `{total_tenants, total_idp_configs, passing, warnings_only, failing, failing_tenants[]}`.

---

## 8. §11 Observability

**Metrics (all `iam_realm_provisioner_*`, §11.2):**
```
iam_realm_provisioner_provision_total{kind=trial|direct|convert, result}          -- counter
iam_realm_provisioner_users_migrated_total                                       -- counter
iam_realm_provisioner_realm_active_gauge{strategy}                               -- gauge
iam_realm_provisioner_keycloak_call_duration_seconds{op}                         -- histogram
iam_realm_provisioner_keycloak_call_failures_total{op}                           -- counter
iam_realm_provisioner_admin_token_refresh_total{trigger=proactive|401}           -- counter
iam_realm_provisioner_idp_validation_total{outcome}                              -- counter
iam_realm_provisioner_mfa_role_sync_total{action=assign|revoke}                  -- counter
iam_realm_provisioner_realm_config_pending_gauge                                 -- gauge
iam_realm_provisioner_trial_expiry_processed_total                              -- counter
iam_realm_provisioner_trial_expiry_deferred_total                               -- counter (MUST increment on every defer)
iam_realm_provisioner_lapse_processed_total{transition=suspend|offboard}        -- counter
iam_realm_provisioner_realm_export_total{result}                                -- counter
iam_realm_provisioner_user_delete_total{result}                                 -- counter (RP-15)
iam_realm_provisioner_session_revoke_total{result}                              -- counter (RP-16)
iam_realm_provisioner_mfa_reset_total{result}                                   -- counter (RP-9, fail-closed semantics)
iam_realm_provisioner_template_drift{realm_version}                             -- gauge/counter, report-only (RP-3)
```
Plus passthrough `http_*`/`events_*` (incl. `outbox_dead_letters_total`)/`sqs_*` from the platform
libs. No projection-lag metric (RP is a producer, not a projector).

**Tracing (§11.3).** `gincommon.InitTracingFromEnv()` in both binaries (always; OTLP export is a no-op until
`OTEL_EXPORTER_OTLP_ENDPOINT` is set). `pgcommon.Config.Tracer` on both pools for `db.query` spans. W3C `traceparent`
injected on every outbound HTTP call (O&M, OpenBao, Keycloak) by `adapter/outbound/httpx`; SNS carries `trace_id` on
the outbox envelope instead, so provision→event→O&M-projection is one trace. Keycloak spans named `keycloak.<op>`
(e.g. `keycloak.create_realm`, `keycloak.migrate_user`); CronJob runs rooted at `reconciler.<job>`.

**Logging.** Structured (`platform-gincommon/pkg/logger`), fields: `tenant_id`, `operation`,
`idempotency_key`, `keycloak_realm`, `actor`. NEVER log admin token, password seed, or IdP secret.

**Dashboards and alerts (§11.5).** Grafana "IAM — Realm Provisioner": provision throughput/success by kind,
conversion duration + users-migrated, KC call latency/error by op, admin-token refresh rate, IdP
validation outcomes, MFA-role sync rate, realm-config pending gauge, trial-expiry/lapse
processed+deferred, realm-export success, outbox/SQS/DLQ depths.

**Alerts (`deploy/monitoring/app-alerts.yml`, §11.5):**
- `keycloak_call_failures_total` rate > 5% for 5m — warning
- `trial_expiry_deferred_total > 0` sustained 1h — warning
- `realm_config_pending_gauge > 0` sustained 30m — warning
- `realm_export_total{result="fail"} > 0` — **critical**
- `outbox_dead_letters_total` increase > 0 over 30m — warning
- `{membership,billing,tenant}-realm-q` DLQ depth > 0 — warning
- `admin_token_refresh_total{trigger="401"}` spike — possible credential rotation issue

---

## 9. §12 Configuration (env vars)

| Var | Meaning |
|---|---|
| `APP_ENV`, `APP_PORT` (8080), `METRICS_PORT` (9090) | environment; ports |
| `PG_HOST/PORT/USER/DBNAME/SSLMODE`, `PG_MAX_CONNS`, `PG_BOUNCER_MODE`, `PG_STATEMENT_TIMEOUT`, `PG_SLOW_QUERY_THRESHOLD` | pgcommon DB config |
| `AWS_REGION`, `GLUE_REGISTRY_TENANT_NAME` (`iam-tenant-events`) | region + shared Glue registry |
| `OUTBOX_POLL_INTERVAL` (500ms), `OUTBOX_BATCH_SIZE` (50), `OUTBOX_MAX_ATTEMPTS` (5), `OUTBOX_DRAIN_TIMEOUT`, `OUTBOX_PUBLISH_CONCURRENCY`, `OUTBOX_PUBLISH_TIMEOUT`, `OUTBOX_PRUNE_*` | outbox runner tunables |
| `PROCESSED_EVENTS_TTL_DAYS` | consumer dedup retention (> 7 days) |
| `KEYCLOAK_BASE_URL` (`https://auth.bcbpsolutions.com`), `KEYCLOAK_ADMIN_REALM` (`master`), `KEYCLOAK_ADMIN_CLIENT_ID` (`iam-realm-provisioner`), `KEYCLOAK_ADMIN_CLIENT_SECRET` (from OpenBao via ESO), `KEYCLOAK_RATE_LIMIT_RPS` (20), `KEYCLOAK_TIMEOUT_MS` (8000) | Keycloak Admin API client |
| `BAO_ADDR` (`https://openbao.iam.svc.cluster.local:8200`), `BAO_AUTH_ROLE` (`iam-realm-provisioner`), `BAO_KV_MOUNT` (`secret`), `BAO_IDP_PATH_PREFIX` (`iam/realm-provisioner/idp`), `BAO_KEYCLOAK_ADMIN_PATH` (`iam/realm-provisioner/keycloak-admin`) | OpenBao — auth method is `kubernetes` (pod SA token) |
| `SHARED_TRIAL_REALM` (`trial`), `REALM_TEMPLATE_VERSION` (`"v3"`) | trial realm + versioned template |
| `ORG_MEMBERSHIP_BASE_URL` | O&M client (I-8 check) — fail-fast on dev default outside `APP_ENV=dev` |
| `REALM_EXPORT_BUCKET` (`iam-realm-exports`), `REALM_EXPORT_KMS_KEY` (`alias/iam-realm-export`) | offboarding export target |
| `TRIAL_GRACE_DAYS` (30), `CANCELLED_GRACE_DAYS` (30), `RETENTION_DAYS` (90) | lifecycle timers |
| `DOCS_ENABLED`, `DOCS_AUTH_TOKEN` | AsyncAPI/OpenAPI viewer gating |
| cron schedules (Helm-level, not necessarily env): `trialExpirySchedule="0 2 * * *"`, `trialHardDeleteSchedule="0 3 * * *"`, `subscriptionLapseSchedule="0 2 * * *"`, `realmConfigReconcile="*/10 * * * *"`, `batchLimit=100` |
| `cache: keyspace="rp:"`, `adminTokenSafetyMarginSeconds=30`, `idempotencyTtlSeconds=86400` | Valkey cache config |

**Docs gating (§12.1).** `/asyncapi` mounted always outside `APP_ENV=production`; opt-in via
`DOCS_ENABLED` in prod; optionally bearer-gated via `DOCS_AUTH_TOKEN`.

**Fail-fast (§12.1).** `cmd/server/config.go` panics on startup if `KEYCLOAK_BASE_URL` /
`KEYCLOAK_ADMIN_CLIENT_SECRET` / `ORG_MEMBERSHIP_BASE_URL` are empty or dev-default outside `APP_ENV=dev`.

**Realm template (§12.2, RP-3).** In-repo versioned `deploy/keycloak/realm-template.json`
(IAM-Platform-owned, PR-reviewed). `REALM_TEMPLATE_VERSION` selects active version, embedded/mounted
at deploy time (not S3/external). Promotion: CI/CD dev→staging→prod gated by Keycloak import
validation + realm-creation smoke tests. **No retroactive application** — version bump only affects
newly provisioned realms; no bulk re-template job in MVP. **Drift detection: yes** (report-only, no
auto-remediation) — compare each realm's `tenant_realms.realm_template_version` against current
`REALM_TEMPLATE_VERSION`, surface via `iam_realm_provisioner_template_drift{realm_version}`.

---

## 10. §13 Deployment and Scaling

**Topology.** EKS namespace `iam` (ap-south-1); RDS `provisioner` (Multi-AZ, PgBouncer tx pooling);
ElastiCache Valkey (`rp:`); SNS topic `iam-tenant-events` + 3 consumer SQS queues each with DLQ; Glue
registry `iam-tenant-events`; S3 bucket for realm exports; one Envoy upstream (mesh-internal only).

**Two workloads from one image:** `server` Deployment (API + 3 SQS consumers + outbox runner) and
`reconciler` run as K8s CronJobs (`cmd/reconciler --job=<name>`).

**Scaling.** `replicaCount: 2` (manual, low-volume per HLD §5.2). HPA conservative (min 2/max 4,
CPU+memory). Keycloak Admin API (not RP's CPU) is the real bottleneck during mass migration — governed
by the client-side rate limiter (§9.4).

**Helm chart `deploy/helm/realm-provisioner/`** (mirror sibling charts): `deployment.yaml`,
`cronjobs.yaml` (4 reconciler jobs + 2 prune jobs), `hpa.yaml`, `pdb.yaml`, `service.yaml` (+metrics
port), `serviceaccount.yaml` (IRSA annotation), `servicemonitor.yaml`, `prometheusrule.yaml`,
`networkpolicy.yaml` (monitoring-ns scrape + mesh egress to Keycloak/O&M/OpenBao/AWS),
`securitypolicy.yaml` (Envoy internal-route auth), `secret.yaml` (ESO `ExternalSecret` refs against
OpenBao `ClusterSecretStore` for deploy-time pod secrets — DB password, Keycloak admin credential),
`httproute.yaml`.

`deploy/iam/` — pod IAM policy (`policy.json` + `.tf.example`) for **AWS resources ONLY**:
`sns:Publish` to `iam-tenant-events`; `sqs:*` on 3 queues + DLQs; `glue:GetSchemaVersion` on
`iam-tenant-events` registry; `s3:PutObject`/`GetObject` on export bucket; `kms:*` on export key.
**Secret access is NOT in this IAM policy** — governed entirely by OpenBao.

`deploy/openbao/` — `policy.hcl` (grants on `secret/data/iam/realm-provisioner/idp/*` +
`secret/data/iam/realm-provisioner/keycloak-admin`) + `role.tf.example` (Kubernetes-auth role binding
the pod ServiceAccount).

`deploy/keycloak/realm-template.json` — versioned template (§12.2).

`deploy/monitoring/` — `app-alerts.yml`, `slo-rules.yml`, `prometheus-adapter-rule.yaml`.

**CI/CD (`.github/workflows/`, §13.5)** — copy sibling pipeline set:
- `validate-test.yml` — unit+integration (`go test -race`), coverage gate (`.github/scripts/coverage-gate.sh`), Postgres + Keycloak testcontainers.
- `validate-quality.yml` — `golangci-lint`, `.go-arch-lint.yml` check (`.github/scripts/arch-lint.sh`), no-session-SET-GUC grep, no-gocloak-outside-`adapter/keycloak` grep (RP-INV-1), **AsyncAPI-vs-Glue drift gate** (pinned `platform-schemagov` validation of `api/asyncapi.yaml` against the `iam-tenant-events` Glue registry — fails PR on drift).
- `ci.yml` — orchestrates the above, `build-image` (multi-stage Dockerfile + provenance scripts), `trivy` scan, `smoke` (`/healthz` 200, `/asyncapi.yaml` serves real spec), `pr-summary`, push to GHCR on default branch.
- `release.yml` — semver tag → release binary+image.
- `changelog-check.yml` — enforces `CHANGELOG.md` `[Unreleased]` entry per PR.
- Plus `.github/CODEOWNERS`, `dependabot.yml`, `ISSUE_TEMPLATE/`, `SECURITY.md`, `pull_request_template.md`.

**Rollout (§13.6).** `golang-migrate` pre-deploy Job (tx-wrapped) via `pgcommon/pkg/migrate`, role
`provisioner_migrator`. Graceful shutdown: drain HTTP, let in-flight SQS messages finish
(at-least-once + `processed_events` makes redelivery safe), let outbox runner finish current batch
(`OUTBOX_DRAIN_TIMEOUT`). AWS access via IRSA (no static keys); OpenBao access via Kubernetes auth
using the SAME ServiceAccount token (no AWS creds involved in secret access).

---

## 11. §14 Testing Strategy

**Unit (§14.1).** Provisioning-flow branches (trial/direct/convert/revert); KC-effect-first ordering
(local commit only after KC step succeeds; compensating teardown on partial failure); idempotency
ledger replay (same key → stored result, NO second KC call); reconcile-based MFA-role handler
(converges under redelivery/reordering, RP-EVT-6); `TenantReactivated` handler no-ops on non-suspended
tenant (RP-EVT-7); §11.6 validator (each CRITICAL blocks 400, each WARN allows only w/ recorded ack);
realm-config apply→reconcile-on-KC-failure; shard-placement tie-break determinism. `KeycloakAdmin` port
mocked — assert EXACT gocloak call shapes (realm clone, sub-preserving user create, TOTP copy,
WebAuthn required-action flag, forced-logout).

**Integration (§14.2).** Postgres testcontainer w/ RLS (repo CRUD, RLS matrix §14.5, `processed_events`
dedup, outbox enqueue/publish/prune, `realm_config_reconciliations` retry) AND a **Keycloak
testcontainer** for the `keycloak` adapter: real realm clone, user create w/ sub preservation, TOTP
copy, IdP create w/ attribute mappers, `requires-mfa` role assign/revoke, realm disable/enable, realm
export. Assert admin-token caching + single-flight refresh against real token endpoint, rate limiter
under simulated migration burst.

**Contract (§14.3).** O&M `OrgMembershipClient` mock (I-8 shape) + test that trial-expiry sweeps ONLY
local `tenant_realms.trial_ends_at`, never calls O&M; `TrialStarted` consumer upserts `trial_ends_at`;
9 published-event payloads validated against `internal/eventschema/*.json` AND `api/asyncapi.yaml`
(incl. `direct_signup` on `TenantConverted`, `export_s3_key` on `TenantOffboarded`, `source` on
`TenantSuspended`/`TenantReactivated`); consumed-event decodes against producers' schemas; test that
produced vs consumed `TenantReactivated` are distinguished by `source` (RP-D13); RP-13 report shape
matches HLD verbatim. AsyncAPI: validate all 13 messages (9 send + 4 receive) and two-producer
registry-sharing (no name collision w/ O&M's `TenantCreated`/`TrialStarted`).

**E2E (§14.4).** Trial provision → (Signup's I-1 creates O&M row) → `TrialTenantProvisioned` settles
it; convert → users migrated (TOTP copied, WebAuthn flagged) → shared sessions force-logged-out →
`TenantConverted` → AuthZ cache invalidated + O&M flips realm/strategy/status; IdP configure w/
region-pinned URL → `400` blocked, region-neutral → `200`; approver grant → `membership-realm-q` →
`requires-mfa` assigned; billing lapse crons → `TenantSuspended` (realm disabled) → past retention →
realm exported to S3 → `TenantOffboarded` (terminal); `TenantReactivated` on suspended tenant
re-enables realm, on cancelled tenant no-ops.

**RLS test cases (§14.5, canonical — MUST implement all 5):**
```sql
-- Case 1: Missing GUC → 0 rows
RESET app.tenant_id;
SELECT count(*) FROM tenant_realms;                                   -- expect: 0

-- Case 2: Cross-tenant write rejected
SET LOCAL app.tenant_id = 'aaaaaaaa-...';
INSERT INTO keycloak_idp_configs (tenant_id, idp_alias, protocol, display_name, config)
VALUES ('bbbbbbbb-...', 'acme', 'saml', 'Acme', '{}');               -- expect: ERROR

-- Case 3: Cross-tenant read isolation
SET LOCAL app.tenant_id = 'aaaaaaaa-...';
SELECT count(*) FROM tenant_realms WHERE tenant_id = 'bbbbbbbb-...'; -- expect: 0

-- Case 4: Cross-tenant update rejected
SET LOCAL app.tenant_id = 'aaaaaaaa-...';
UPDATE tenant_realms SET status = 'disabled' WHERE tenant_id = 'bbbbbbbb-...';  -- expect: 0 rows
```
Case 5 — `TestRLS_NoGUCLeakageAcrossPooledConnection` (RLS-6): pin one backend (`MaxConns=1`); tenant A
writes under `SET LOCAL`; tenant B reads → sees only B's rows; tenant B on same backend w/ no GUC set →
reads 0 (fail-closed via `NULLIF`). Applies to all 5 tenant-scoped tables + reconciler/consumer write
paths. CI greps for forbidden non-`LOCAL` `SET app.tenant_id`.

**Load (§14.6).** Migration load test (`test/load/` convention): convert synthetic tenant w/ N users
(e.g. 500), assert client-side rate limiter keeps KC Admin API call rate in budget, conversion time
scales linearly; report p50/p95/p99 per-user migration latency (loopback/mock-KC caveat noted).

---

## 12. §15 GDPR / Data Lifecycle

**PII (§15.8).** `provisioner` DB holds minimal PII: `keycloak_idp_configs.config` may have
organisational (not personal) URLs/entity IDs; `provisioning_operations.result` transiently mirrors a
provisioning response (can include `admin_email`). Real PII lives in Keycloak (not mirrored). No
personal fields in `tenant_realms`.

**Erasure (§15.5):**
- Trial expiry PII scrub — at 30-day-grace hard-delete: hard-delete trial user from shared `trial`
  realm via Admin API (removes email/name/profile), soft-delete local row, scrub residual PII,
  retain only `tenant_id` for audit-FK. Immutable `sub` preserved for audit continuity.
- Tenant offboarding — dedicated realm exported to S3 encrypted (KMS) for compliance retention, THEN
  hard-deleted from Keycloak (removes all users/credentials), local row soft-deleted/scrubbed.
  `TenantOffboarded` carries `export_s3_key`. Realm delete NEVER precedes confirmed export (RP-EVT-4).
  `offboarded` is terminal (RP-INV-5).

**Residency.** Single-region ap-south-1 (RDS/ElastiCache/S3). §11.6 gate exists to keep customer-side
IdP config from pinning the platform to a region (future Mumbai→UAE migration).

**Retention.** `tenant_realms` live rows persist for tenant's life; soft-deleted retained for audit-FK,
pruned per platform schedule. `keycloak_idp_config_validations` append-only. S3 exports follow
compliance schedule (7yr for approval/signature-adjacent records). `outbox_events`/`processed_events`
pruned on operational retention timers.

---

## 13. §16 Register — status check (nothing blocks implementation)

All items Resolved except decisions-already-made (not blockers): RP-1 (Resolved, O&M-confirmed), RP-2
(Resolved, Keycloak-ops-confirmed), RP-3 (Resolved, IAM-Platform+SRE-confirmed), RP-4 (Resolved,
O&M-confirmed both sides), RP-5 (Decision: local mirror for shard-load — not open), RP-6 (Resolved,
O&M-confirmed; 1 non-blocking O&M-side doc-gap), RP-7 (Decision: WebAuthn UX is web-team's, out of
scope here), RP-8 (Decision: late-revert runbook authored separately, §24), RP-9 (Resolved — name
freeze DONE, v1.0), RP-10 (Resolved — HLD v1.42 OpenBao alignment), RP-11 (Resolved, O&M-confirmed both
sides — `suspension_source` schema change made), RP-12 (Resolved via RP-D13), F5 (Resolved both
sides — O&M sends `required_actions`), F6/OQ-8 (Resolved — built both sides, O&M P-34 + `ResetMFA`
live). **No unresolved blocking item found.** Non-blocking O&M-side doc-sync notes (e.g. missing
direct-paid flow diagram on O&M's LLD) do not affect this service's implementation.

---

## 14. §17 Full Error Taxonomy

`platform-gincommon.ErrorResponse` flat wire shape:
```json
{ "error": "idp_region_portability_failed", "status": 400, "trace_id": "abc123", "request_id": "req-xyz", "details": { "failed_checks": ["check_1_sso_url_region"] } }
```

| `code` | HTTP | Meaning | `details` |
|---|---|---|---|
| `invalid_realm_strategy` | 400 | `strategy` not in `{shared, dedicated}` | — |
| `invalid_idp_protocol` | 400 | `protocol` not in `{saml, oidc}` | — |
| `unsupported_realm_setting` | 400 | `setting_key` not in the realm-affecting allow-list | `details.allowed[]` |
| `idp_region_portability_failed` | 400 | one or more §11.6 CRITICAL checks failed | `details.failed_checks[]` |
| `missing_idempotency_key` | 400 | mutating call without `Idempotency-Key` | — |
| `idp_warnings_unacknowledged` | 422 | §11.6 WARN item(s) not acknowledged | `details.warnings[]` |
| `tenant_realm_not_found` | 404 | no `tenant_realms` row for tenant in caller scope | — |
| `idp_config_not_found` | 404 | no such `(tenant, idp_alias)` | — |
| `idp_requires_dedicated_realm` | 409 | IdP config attempted on shared-realm (trial) tenant | — |
| `realm_already_dedicated` | 409 | convert/direct-provision on already-dedicated tenant | — |
| `revert_window_expired` | 409 | revert-conversion >7 days after conversion | `details.converted_at` |
| `tenant_offboarded` | 409 | any mutation on `offboarded` (terminal) tenant | — |
| `operation_in_progress` | 409 | concurrent duplicate of same `Idempotency-Key` running | — |
| `optimistic_lock_conflict` | 409 | `record_version` mismatch | `details.record_version` |
| `keycloak_unavailable` | 502 | KC Admin API 5xx/timeout; no local commit; retryable | — |
| `conversion_failed` | 502 | partial provisioning compensated; shared realm still authoritative | — |
| `org_membership_unavailable` | 503 | O&M query 5xx/timeout; retryable | — |
| `object_store_unavailable` | 503 | realm-export S3 write failed; offboarding deferred | — |
| `db_unavailable` | 503 | Postgres connectivity / resource SQLSTATE (08/53/57/58) | — |

---

## 15. §18 Integration Details

**O&M `RealmProvisionerClient` → RP endpoint mapping (§18.3, byte-exact signatures required):**

| O&M method | RP endpoint | Notes |
|---|---|---|
| `CreateInvitedUser(ctx, req)` | RP-5 `POST …/users` | `{email, full_name, required_actions}`; RP applies verbatim |
| `DeleteUser(ctx, tenantID, keycloakUserID)` | RP-15 `DELETE …/users/:keycloak_user_id` | idempotent, `204` always |
| `RevokeUserSessions(ctx, tenantID, keycloakUserID)` | RP-16 `POST …/revoke-sessions` | best-effort/fail-open on O&M's side |
| `PatchRealmConfig(...)` | RP-8 `PATCH …/realm-config` | 200/202 |
| `ResetMFA(ctx, tenantID, keycloakUserID uuid.UUID) error` | RP-9 `POST …/mfa-reset` | fail-closed (RP `502`→O&M `503`) |

All calls: mesh-mTLS, **system principal** `x-user-id=…00a1` + target `x-tenant-id` header (O&M
RLS-5/F10). Cross-check exact signatures against
`iam-org-membership`'s `internal/core/port/outbound_clients.go` before finalizing handler contracts.

**O&M I-8** — `GET /api/v1/internal/users/:id/memberships` — RP calls this on the `TenantRoleRevoked`
drop path only (residual MFA-requirement check, §8.6).

**Realm-id/shard reach O&M via events, not sync call (RP-D14, F4).** O&M's `realm_id`/`realm_type`/
`keycloak_shard` projection comes from consuming `TenantRealmReady`/`TenantConverted` (payloads carry
`realm`/`keycloak_shard`) — RP never calls O&M's I-2.

**Tenant-row creation is O&M's I-1, called by Signup — NOT by RP (RP-6).** RP's
`TrialTenantProvisioned`/`DirectPaidSignup` events settle/reconcile the already-created row; they do
not create it and never relay tenant metadata (name/slug/plan).

**Signup Service (§18.1).** Calls RP-1/RP-2. Pre-allocates `tenant_id` UUID, passes identical value to
Keycloak `tenant_id` attribute (via RP) AND O&M's I-1 `id` (idempotent on `id`).

**Billing (§18.2).** Calls RP-3/RP-4. Produces `TenantReactivated` on `billing.events` (RP consumes on
`billing-realm-q`). Billing is authority for `subscription_status`; RP only does the KC-side effect.

**AuthZ Enrichment (§18.5).** Consumes `TenantConverted` on `tenant-authz-q` to invalidate its
realm→tenant cache. RP makes no request-path decision.

**Event Consumer (§18.6).** No direct coupling — handles Keycloak's OUTBOUND webhooks
(`REGISTER`, `IDENTITY_PROVIDER_FIRST_LOGIN`); RP only configures the IdP that enables federated login.

**Keycloak (§18.4).** All mutations via gocloak against `auth.bcbpsolutions.com` (region-neutral),
behind `port.KeycloakAdmin` (RP-INV-1). Never sits in login/token path.

---

## 16. §19 Migration / Provisioning-Ordering Plan

Development-branch build, no live data/traffic to preserve. Order:
1. Stand up RP: `provisioner` DB (5 tenant tables + `outbox_events` + `processed_events`), `/internal`
   API (RP-1..RP-13[..16]), `keycloak` adapter, `iam-tenant-events` publisher (register 9 schemas in
   shared Glue registry alongside O&M's 2), 3 SQS consumers.
2. Wire callers: Signup→RP-1/RP-2; Billing→RP-3/RP-4 + `TenantReactivated` producer; O&M→RP-5..RP-9 +
   `DepartmentMembership*` producer + `tenant-orgm-q` consumer of the 9 events.
3. Wire AuthZ Enrichment: subscribe `tenant-authz-q` to `TenantConverted`.
4. Provision infra: shared topic+registry, 3 queues+DLQs, S3 bucket+KMS key, Keycloak admin SA w/
   RP-2 minimal role set.

**Ordering invariant.** Because every flow is Keycloak-effect-first-then-local-commit, a crash never
leaves a phantom local row — retry with same `Idempotency-Key` reconciles cleanly. Migration-runner
ordering at startup (`cmd/server/main.go`): `outbox.ApplySchema` MUST run BEFORE this service's domain
migration (which ALTERs `outbox_events.payload` and adds the grants/trigger — the table must already
exist).

---

## 17. §22 Decision Register (RP-D1..RP-D14, one-liners)

- **RP-D1** — All logs/metrics/traces through `platform-gincommon` only.
- **RP-D2** — All DB via `platform-pgcommon`; all events/outbox/dedup via `platform-events`.
- **RP-D3** — MFA-role assignment is event-driven (`membership-realm-q`), NOT sync O&M→RP; only MFA *reset* is sync.
- **RP-D4** — Admin token cached in Valkey w/ single-flight refresh.
- **RP-D5** — Publish into the shared `iam-tenant-events` Glue registry alongside O&M (disjoint schema names).
- **RP-D6** — `provisioner` DB is a KC-side mirror + ops metadata, NOT a copy of O&M's `tenants` row.
- **RP-D7** — Every externally-triggered mutation is idempotency-ledgered.
- **RP-D8** — Export-before-delete for offboarding, gated on confirmed S3 write.
- **RP-D9** — Client-side Keycloak Admin API rate limiter + backoff.
- **RP-D10** — Per-tenant IdP secrets via deterministic OpenBao KV path, no DB pointer stored.
- **RP-D11** — Platform secrets backend is OpenBao, not AWS Secrets Manager (both deploy-time via ESO and dynamic IdP secrets via KV API).
- **RP-D12** — RP-14 emergency suspension is effect-only, not `subscription_status` authority.
- **RP-D13** — Paid operator-reactivation reuses `TenantReactivated` w/ `source(operator|billing)` field, not a new event type.
- **RP-D14** — Realm-id/shard reach O&M via events (`TenantRealmReady`/`TenantConverted`), not a sync RP→O&M I-2 call.

---

## 18. §23 Glossary (non-obvious terms)

- **Shared `trial` realm** — the one Keycloak realm ALL trial tenants live in; distinguished by `tenant_id` JWT claim (RP-INV-2).
- **Dedicated realm** — per-paid-tenant realm `tenant-<slug>`, created at conversion/direct-paid.
- **`sub`** — immutable Keycloak user UUID; preserved across trial→dedicated migration.
- **`requires-mfa` realm role** — makes the conditional authenticator force MFA enrollment.
- **Realm template** — in-repo versioned `realm-template.json` cloned to create dedicated realms.
- **Region-portability gate** — §11.6 checklist.
- **Outbox** — `outbox_events`, written in-tx, published async by `outbox.Runner`.
- **`processed_events`** — consumer dedup ledger `(event_id, consumer)`.
- **Shard** — a Keycloak deployment unit; dedicated realms placed on least-loaded shard.
- **OpenBao** — platform secrets store (Vault-API-compatible), replaces AWS Secrets Manager.

---

## 19. §24 Operational Runbooks (topics — full text in `docs/runbook.md`)

1. Keycloak Admin API degraded — retry guidance, admin-token/rate-limit checks.
2. Realm-config not landing — inspect `realm_config_reconciliations`, RP-C4 auto-retries.
3. Offboarding export failed — tenant stays `suspended`, fix S3/KMS, re-run sweep.
4. Late conversion reversal (>7 days) — manual recreate/migrate-back/teardown procedure.
5. Stuck `in_progress` idempotency key — safe recovery is caller retry; force-clear only after confirming no KC partial state.
6. Pre-migration IdP portability audit — run RP-13, must show zero failing tenants before regional cutover.

---

## 20. Cross-checks still required at implementation time

- Verify exact `port.RealmProvisionerClient` Go signatures in
  `iam-org-membership/internal/core/port/outbound_clients.go` byte-for-byte before writing RP-5/9/15/16
  handlers and any client-facing DTOs.
- Verify O&M's `api/asyncapi.yaml` for the exact `membership-realm-q`/`tenant-realm-q` channel/message
  schema field names/types before writing consumer payload structs.
- RP-2's exact request body isn't spelled out as completely as RP-1's in §5.2 — infer parity with RP-1
  (`hashed_password_seed` likely included) but confirm no contradiction exists in the O&M or Signup
  contracts before finalizing the DTO.
