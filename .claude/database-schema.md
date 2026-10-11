# Database Schema

Refines LLD §4, §19. Database `provisioner` on shared RDS PostgreSQL, fronted by PgBouncer in transaction-pooling mode in production (`PG_BOUNCER_MODE=true`). Pool: `pgcommon.NewPool(... GUCProvider: pgcommon.GUCSetFromContext)`. Local dev uses `postgres:16-alpine` (port 5544) in `docker-compose.yml`. Single consolidated migration `000000_initial_schema.{up,down}.sql` (`internal/adapter/outbound/postgres/migrations/`) — this service has never shipped a prior schema version.

## Extensions, Enums, and Helper Functions (§4.1)

```sql
CREATE TYPE realm_strategy     AS ENUM ('shared', 'dedicated');
CREATE TYPE realm_status       AS ENUM ('provisioning', 'active', 'disabled', 'suspended', 'exported', 'deleted');
CREATE TYPE idp_protocol       AS ENUM ('saml', 'oidc');
CREATE TYPE idp_config_status  AS ENUM ('active', 'disabled', 'pending_validation');
CREATE TYPE validation_outcome AS ENUM ('pass', 'warnings_only', 'failing');
CREATE TYPE recon_status       AS ENUM ('pending', 'applied', 'failed');
CREATE TYPE op_status          AS ENUM ('in_progress', 'succeeded', 'failed');
```

Three helper functions back RLS:
- **`app_tenant_id()`** — `STABLE SECURITY DEFINER`, reads `current_setting('app.tenant_id', true)`. Returns `NULL` on any error (including a malformed UUID) — fail-closed, RLS blocks rather than silently opens.
- **`touch_row()`** — `BEFORE UPDATE` trigger function: bumps `updated_at` and `record_version := OLD.record_version + 1`. Attached with `WHEN (OLD.* IS DISTINCT FROM NEW.*)` so a no-op `UPDATE` doesn't bump the version.
- **`rls_check_tenant(p_tenant_id, p_table_name)`** — `STABLE STRICT SECURITY DEFINER`. Returns `true` iff `p_tenant_id = app_tenant_id()`; on mismatch (or missing GUC) logs to `rls_violation_log` via `log_rls_violation` (1% sampled, self-swallowing on error so a logging failure never aborts the caller's transaction) and returns `false`.

## Tables (§4.2-§4.5)

**6 tenant-scoped tables, all under `FORCE ROW LEVEL SECURITY`:**

| Table | Key columns / notable constraints |
|---|---|
| `tenant_realms` | PK `tenant_id` (not a surrogate `id` — one row per tenant). `realm_name`, `strategy` (`realm_strategy`), `keycloak_shard` (nullable, required for `dedicated` — `chk_shard_dedicated`), `status` (`realm_status`, default `provisioning`), `realm_template_version`, `trial_ends_at`, `converted_at`, `disabled_at`, `exported_at`, `export_s3_key`, `service_account_principal_id`/`service_account_keycloak_client_id` (nullable, set once the tenant's `platform-automation` principal is minted at RP-1/RP-2/RP-3, and re-set at RP-4 when reverting a conversion re-mints the client back in the shared realm — §2.5, RP-17), `record_version`. `chk_shared_is_trial` — a `shared`-strategy row's `realm_name` must literally be `'trial'`. `uq_tenant_realms_dedicated_name` — dedicated realm names globally unique (partial, `WHERE strategy='dedicated' AND deleted_at IS NULL`). Indexes: `idx_tenant_realms_status`, `idx_tenant_realms_trial_exp` (`WHERE strategy='shared' AND status='active'` — backs RP-C1's `ListExpiredTrials`), `idx_tenant_realms_strategy`. |
| `keycloak_idp_configs` | Surrogate `id`. `tenant_id`, `idp_alias`, `protocol` (`idp_protocol`), `display_name`, `enabled`, `config jsonb` (**non-secret** protocol params only — the secret itself lives in OpenBao, never here), `status` (`idp_config_status`), `record_version`. `uq_idp_tenant_alias UNIQUE (tenant_id, idp_alias)`. |
| `keycloak_idp_config_validations` | Surrogate `id`. Append-only, one row per §11.6 validation run: `tenant_id`, `idp_alias`, `validator_version`, `outcome` (`validation_outcome`), `checks jsonb` (per-check-ID outcome map), `acknowledgements jsonb` (WARN check IDs the admin explicitly acked), `validated_at`. No `record_version` (append-only, never updated). Index `idx_idp_validations_tenant (tenant_id, idp_alias, validated_at DESC)`. |
| `realm_config_reconciliations` | Surrogate `id`. RP-C4's retry ledger: `tenant_id`, `setting_key`, `desired_value jsonb`, `status` (`recon_status`, default `pending`), `attempts`, `last_error`, `created_at`, `applied_at`. Index `idx_recon_pending (created_at) WHERE status='pending'`. |
| `provisioning_operations` | PK `idempotency_key` (text, caller-supplied via the `Idempotency-Key` header) — **not** a UUID surrogate. `tenant_id`, `operation` (text — e.g. `provision_trial`, `convert`), `status` (`op_status`, default `in_progress`), `result jsonb` (the marshaled response body, stored on success for replay), `created_at`, `completed_at`. Indexes `idx_provops_tenant (tenant_id, created_at DESC)` and `idx_provops_created_at (created_at)` (the `ledger-prune` sweep, 30 days). An `in_progress` row older than `IDEMPOTENCY_IN_PROGRESS_LEASE` is reclaimable by a retry. RLS-scoped like the other tenant-scoped tables (not exempt like `processed_events`) **because its `result` payloads can embed tenant data**. |
| `pending_audit_entries` | Surrogate `id` — also the AL-5 `Idempotency-Key`, stable across delivery retries. Durable hand-off of the non-bus `TenantIdpConfigChanged` audit entry to Audit Log's AL-5 direct-write ingest (LLD §10.6, RP-D15): `tenant_id`, `entry jsonb` (the AL-5 request body verbatim), `status` (text `CHECK IN ('pending','failed')`), `attempts`, `last_error` (capped at 1024 bytes, cut on a UTF-8 boundary and sanitized — an invalid byte sequence would make Postgres reject the status write), `next_attempt_at` (backoff + claim lease), `created_at`. Inserted by RP-6/RP-7 **in the same `RunInTx`** as the `keycloak_idp_configs` write (app pool, RLS-bound); claimed/marked by the `cmd/server` drainer on the BYPASSRLS `sysPool` (`ClaimDue` = one `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)`). Delivered rows are **deleted**; permanently rejected rows stay `failed`. App role gets `SELECT, INSERT` only. Index `idx_pending_audit_due (next_attempt_at) WHERE status='pending'`. RLS-scoped because `entry` embeds tenant data. **Prod grant:** `realm_provisioner_migrator` needs `SELECT, UPDATE, DELETE` on it — Terraform-owned, like its other table grants. |

**3 RLS-exempt operational tables:**

| Table | Notes |
|---|---|
| `outbox_events` | Created by `platform-events`' `outbox.ApplySchema`, **not** this migration — this migration only customizes it: `ALTER COLUMN payload TYPE text` (was jsonb) + a `trg_outbox_normalize_payload` trigger that decodes pgx SimpleProtocol's bytea-hex (`\x...`) encoding back to UTF-8 on every INSERT, since PgBouncer forces SimpleProtocol and pgx encodes `[]byte` as bytea-hex even for a `text` column. |
| `processed_events` | Defined in this migration in exactly platform-events' `pkg/inbox` shape — `event_id uuid`, PK constraint `processed_events_pkey (event_id, consumer)` — and read/written **only** through `inbox.Store` (`postgres.InboxRepository`; arch-lint forbids another writer). `consumer` is one of `'membership'`\|`'tenant'`\|`'billing'` (the three SQS consumers). No tenant scoping, no FK, no RLS — a pure global dedup ledger. Index `idx_processed_events_processed_at`. |
| `exporter_watermarks` | PK `exporter_name` (text). `last_processed_at timestamptz`. One row per `cmd/server` metric exporter that scrapes an append-only table on a persisted high-water mark instead of a fixed trailing window (today: `rls_violations_total`'s exporter). `GaugeRepository.RLSViolationCounts` takes `SELECT ... FOR UPDATE` on the row so concurrent server replicas (HPA `minReplicas: 2`) serialize on it instead of every replica redundantly re-counting the same `rls_violation_log` rows on every tick — and so a pod restart resumes from the mark instead of re-scraping (and re-`Add`ing onto the Prometheus counter) a window the dying pod already counted. Not admin_readonly-granted (unlike the 8 tables it does grant) — it carries no tenant or compliance-relevant data, just an exporter cursor. |

**1 audit-only table (RLS disabled by design):**

| Table | Notes |
|---|---|
| `rls_violation_log` | `bigserial` PK. `table_name`, `row_tenant_id`, `app_tenant_id`, `violation_type` (`'missing_or_invalid_guc'`\|`'cross_tenant_access'`), `session_role`, `client_addr`, `application_name`, `query_text`, `occurred_at`. RLS deliberately **disabled** on this table — `rls_check_tenant`/`log_rls_violation` write to it and must not recurse into their own policy check. 1% sampling (`random() > 0.01 THEN RETURN`) inside `log_rls_violation`. Index `idx_rls_violation_log_occurred_at (occurred_at)` serves both the `rls_violations_total` exporter's watermark window and the `ledger-prune` sweep (90 days). |

## Row-Level Security (§4.2-§4.4, §14.5)

Every one of the 6 tenant-scoped tables:
```sql
ALTER TABLE public.<table> ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.<table> FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON public.<table> FROM PUBLIC;
CREATE POLICY tenant_isolation ON public.<table>
    USING      (rls_check_tenant(tenant_id, '<table>'))
    WITH CHECK (rls_check_tenant(tenant_id, '<table>'));
```
No special-case policy shape (unlike some sibling services' `tenants` table matching on `id`) — every one of these 6 tables carries its own `tenant_id` column matched against the GUC.

**Cross-tenant admin access** requires the `BYPASSRLS` role **`realm_provisioner_migrator`** — never the app role `realm_provisioner_app`. The migration's final `DO $$ ... $$` block actively **strips** `BYPASSRLS` from `realm_provisioner_app` if it's ever found set (`ALTER ROLE realm_provisioner_app NOBYPASSRLS`), logging a `WARNING` if it lacks privilege to fix it — a defense-in-depth self-heal, not just a one-time grant.

A third role, **`admin_readonly`** (`NOLOGIN BYPASSRLS`), exists purely for cross-tenant compliance/support `SELECT`s across all 8 tables (6 RLS-scoped + `rls_violation_log` + `processed_events`) — never used by application code.

**RLS invariants (canonical test cases, `test/postgres/rls_test.go`):**
- **Case 1** — Missing/malformed GUC → `app_tenant_id()` returns `NULL` → `rls_check_tenant` returns `false` → 0 rows visible, no writes possible.
- **Case 2** — Cross-tenant `INSERT`/`UPDATE` rejected by `WITH CHECK`.
- **Case 3** — Cross-tenant `SELECT` returns 0 rows (`USING` clause).
- **Case 4** — Cross-tenant `UPDATE` (matching a different tenant's row by primary key) affects 0 rows.
- **Case 5 (critical)** — No GUC leakage across a pooled PgBouncer connection: tenant A's transaction sets the GUC transaction-locally (`SET LOCAL`/`is_local=true`), returns the connection to the pool at COMMIT, and a subsequent transaction for tenant B on the **same physical backend** must not inherit tenant A's GUC. This test specifically pins the test pool's `PGBouncerMode: true` to exercise the transaction-scoped `injectGUCsInTx` code path in `platform-pgcommon` (`is_local=true`, auto-resets at commit) rather than the session-scoped `InjectGUCs` path (`is_local=false`, applied at connection-acquire, which does **not** clear a prior tenant's GUC on a no-GUC checkout) — a real, documented behavior difference in the shared library that only surfaces under `PG_BOUNCER_MODE=true`, exactly how production runs.
- **Extra** — `TestRLS_EveryTenantScopedTableEnabled` (structural: all 5 tables have `rowsecurity=true AND forcerowsecurity=true`) and `TestRLS_OperationalTablesExempt` (`outbox_events`/`processed_events`/`exporter_watermarks` deliberately do NOT have RLS enabled).

RLS-6 (no session-scoped `SET app.tenant_id`) is additionally enforced structurally in CI by `.github/scripts/arch-lint.sh`'s grep — see `architecture.md`.

## Triggers

`touch_row()` — `BEFORE UPDATE ... FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)` on `tenant_realms` (`trg_touch_tenant_realms`) and `keycloak_idp_configs` (`trg_touch_keycloak_idp_configs`) — the two tables that carry `record_version`. `keycloak_idp_config_validations`, `realm_config_reconciliations`, and `provisioning_operations` do **not** carry `record_version` and have no touch trigger (append-mostly / status-flip tables, not optimistic-locked entities).

`trg_outbox_normalize_payload` — `BEFORE INSERT ON outbox_events`, decodes pgx's bytea-hex encoding of the `payload` text column back to UTF-8 (see `outbox_events` above).

## Migrations

- `MIGRATION_DATABASE_URL` — a direct (non-PgBouncer) Postgres DSN, required even outside `PG_BOUNCER_MODE`, because `realm_provisioner_app` (the runtime role) has no `CREATE` grant on schema `public`; only `realm_provisioner_migrator` can run `outbox.ApplySchema` and this domain migration.
- **Ordering invariant (§19):** `cmd/server/main.go` runs `outbox.ApplySchema` **before** `000000_initial_schema.up.sql` — the domain migration's `ALTER TABLE outbox_events ALTER COLUMN payload TYPE text` statement requires `outbox_events` to already exist.
- Role/grant statements are wrapped in `DO $$ ... EXCEPTION WHEN insufficient_privilege ... END$$` blocks so the migration runs safely in dev (where the migrator user typically lacks `CREATEROLE`) and in production (where Terraform owns `CREATE ROLE`) alike — every block emits a `NOTICE`/`WARNING` and continues rather than failing the migration.
- Grants to `realm_provisioner_app`: full `SELECT/INSERT/UPDATE/DELETE` on `tenant_realms` and `keycloak_idp_configs`; `SELECT/INSERT` only on `keycloak_idp_config_validations` (append-only); `SELECT/INSERT/UPDATE` on `realm_config_reconciliations` and `provisioning_operations` (no `DELETE` — nothing app-level deletes these rows; pruning, where it exists, runs under the migrator/sys pool); `SELECT/INSERT/DELETE` on `processed_events`; and (guarded `IF EXISTS`) full grants on `outbox_events`.

## Idempotency & Dedup (§9.2)

- **`provisioning_operations`** — every externally-triggered mutating call (RP-1..RP-5, RP-8/RP-10/RP-12/RP-14) is keyed by the caller-supplied `Idempotency-Key` header via `IdempotencyStore.Begin`. `Begin` returns `(existing, found, err)`: `found=false` → proceed and eventually `Complete`/`Fail`; `found=true` + `existing.Status == succeeded` → decode and replay `existing.Result` verbatim (`replayOrConflict` in `provisioning_service.go`); `found=true` + `in_progress` → `409 operation_in_progress` (the caller retries with the same key). `Begin`'s upsert reclaims a `failed` row, or an `in_progress` row whose `created_at` is older than `IDEMPOTENCY_IN_PROGRESS_LEASE` (15m — its pod died mid-operation), and returns `found=false`, so the retry re-executes; every operation behind the ledger is a Keycloak-effect-first reconcile, so re-running converges. `Complete`/`Fail` write on a context detached from the request (10s), so a request deadline or client disconnect never strands a key `in_progress`. Rows are pruned after `PROVISIONING_OPERATIONS_TTL_DAYS` (30) by `ledger-prune`. See `request-flows.md` for the full sequencing against the Keycloak-effect-first discipline.
- **`processed_events`** — the three SQS consumers (`membership-realm-q`, `tenant-realm-q`, `billing-realm-q`) each check-then-mark against this table keyed by `(event_id, consumer)` to make at-least-once SQS delivery idempotent.

## Data Not Stored Here — By Design

- **No secret material, ever** (§10.2/§12). Realm passwords and trial-user seed credentials live only in Keycloak. `keycloak_idp_configs.config` holds only non-secret SAML/OIDC parameters — the IdP's client secret / SP signing key lives exclusively in OpenBao at `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`, passed through this service transiently (`port.IdpKeycloakConfig.Secret`) and never persisted to Postgres. Same discipline for the `platform-automation` principal's credential (§2.5, EXT-6): Token Service generates and custodies the private key in its own OpenBao and serves its public half as a JWKS; this service never even receives the private key plaintext for a mint-time apply step (that step no longer exists — Keycloak fetches the JWKS lazily) — `tenant_realms` stores only the non-secret `service_account_principal_id`/`service_account_keycloak_client_id` pair.
- **No per-user table.** This service has no `users` table — Keycloak is the sole store of user identity/credentials; `tenant_realms` tracks realm-level state only. RP-5 (`CreateInvitedUser`) resolves the target realm from `tenant_realms` and calls Keycloak directly, with nothing written locally afterward.
- **No tenant business-entity table.** `tenant_realms` is keyed by `tenant_id` (a foreign UUID owned by Org & Membership) but this service stores no tenant name/plan/subscription data of its own — `RP-C3`'s suspend phase polls that state from O&M's I-16 `GET /api/v1/internal/subscription-lapses` via `port.SubscriptionLapseReader`; the durable `suspension_source` (`billing_lapse`|`operator`) column on `tenant_realms` is this service's own record of *why* a realm was suspended, not O&M's business state.
