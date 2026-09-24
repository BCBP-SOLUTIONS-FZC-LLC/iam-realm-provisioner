-- ─────────────────────────────────────────────────────────────────────────
-- Realm Provisioner — initial schema (LLD §4, §19). All names below are
-- frozen by LLD §25 and must match character-for-character.
--
-- outbox_events is created by platform-events' outbox.ApplySchema, which
-- cmd/server/main.go runs BEFORE this migration (§19 ordering invariant) —
-- this migration only customizes it (payload column + trigger + grants) and
-- never defines it. processed_events is this service's own DDL.
-- ─────────────────────────────────────────────────────────────────────────

-- §4.1 enums
CREATE TYPE realm_strategy     AS ENUM ('shared', 'dedicated');
CREATE TYPE realm_status       AS ENUM ('provisioning', 'active', 'disabled', 'suspended', 'exported', 'deleted');
CREATE TYPE idp_protocol       AS ENUM ('saml', 'oidc');
CREATE TYPE idp_config_status  AS ENUM ('active', 'disabled', 'pending_validation');
CREATE TYPE validation_outcome AS ENUM ('pass', 'warnings_only', 'failing');
CREATE TYPE recon_status       AS ENUM ('pending', 'applied', 'failed');
CREATE TYPE op_status          AS ENUM ('in_progress', 'succeeded', 'failed');

-- ─────────────────────────────────────────────────────────────────────────
-- app_tenant_id() — reads the tenant GUC set by pgcommon's GUC bridge
-- (SET LOCAL app.tenant_id, RLS-6). STABLE (evaluates once per statement),
-- SECURITY DEFINER (invoker cannot poison the search path), fail-closed
-- (returns NULL on any error so RLS blocks rather than silently opens).
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.app_tenant_id() RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = public AS $$
DECLARE v text;
BEGIN
    v := current_setting('app.tenant_id', true);
    IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
    RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- touch_row() — BEFORE UPDATE on every record_version-carrying table.
-- Fires only when the row actually changed, so a no-op UPDATE does not
-- bump the version or timestamp.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.touch_row() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at     := now();
    NEW.record_version := OLD.record_version + 1;
    RETURN NEW;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- rls_violation_log / log_rls_violation() — sampled (1%) audit trail for
-- RLS policy failures. RLS is disabled on this table so the logger cannot
-- recurse into itself; it silently swallows its own errors so a logging
-- failure can never abort the caller's transaction.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.rls_violation_log (
    id               bigserial   PRIMARY KEY,
    table_name       text        NOT NULL,
    row_tenant_id    uuid,
    app_tenant_id    uuid,
    violation_type   text        NOT NULL,   -- 'missing_or_invalid_guc' | 'cross_tenant_access'
    session_role     text        DEFAULT SESSION_USER,
    client_addr      inet        DEFAULT inet_client_addr(),
    application_name text        DEFAULT current_setting('application_name', true),
    query_text       text,
    occurred_at      timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION public.log_rls_violation(
    p_table_name     text,
    p_row_tenant_id  uuid,
    p_violation_type text
) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
BEGIN
    IF random() > 0.01 THEN RETURN; END IF;
    INSERT INTO rls_violation_log (
        table_name, row_tenant_id, app_tenant_id, violation_type, query_text
    ) VALUES (
        p_table_name, p_row_tenant_id, app_tenant_id(), p_violation_type, current_query()
    );
EXCEPTION WHEN OTHERS THEN
    NULL;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- rls_check_tenant — STABLE STRICT SECURITY DEFINER. True iff the row's
-- tenant_id matches the app.tenant_id GUC. Logs one of two violation types
-- on failure (missing/invalid GUC, or cross-tenant access).
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.rls_check_tenant(
    p_tenant_id  uuid,
    p_table_name text
) RETURNS boolean
    LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    v_app uuid;
BEGIN
    v_app := app_tenant_id();
    IF v_app IS NULL THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'missing_or_invalid_guc');
        RETURN false;
    END IF;
    IF p_tenant_id <> v_app THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'cross_tenant_access');
        RETURN false;
    END IF;
    RETURN true;
END;
$$;

-- ═════════════════════════════════════════════════════════════════════════
-- §4.2 tenant_realms
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE public.tenant_realms (
  tenant_id              uuid PRIMARY KEY,
  realm_name             text NOT NULL,
  strategy               realm_strategy NOT NULL,
  keycloak_shard         text,
  status                 realm_status NOT NULL DEFAULT 'provisioning',
  realm_template_version text,
  trial_ends_at          timestamptz,
  converted_at           timestamptz,
  disabled_at            timestamptz,
  suspension_source      text,
  exported_at            timestamptz,
  export_s3_key          text,
  -- §2.5/RP-17: the tenant's platform-automation service-account principal,
  -- once minted at RP-1/RP-2/RP-3 — NULL until then. keycloak_client_id is
  -- the literal 'platform-automation' for a dedicated realm, or that name
  -- suffixed with tenant_id for the shared trial realm (a shared realm
  -- cannot hold two clients with the same clientId). principal_id is
  -- Token Service's own row id (TS-4), not a local FK — Token Service owns
  -- that table, not this one.
  service_account_principal_id     uuid,
  service_account_keycloak_client_id text,
  record_version         bigint NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  deleted_at             timestamptz,
  CONSTRAINT chk_shard_dedicated CHECK (strategy = 'shared' OR keycloak_shard IS NOT NULL),
  CONSTRAINT chk_shared_is_trial CHECK (strategy = 'dedicated' OR realm_name = 'trial'),
  -- suspension_source (§25 frozen values, matching TenantSuspended.source):
  -- durably records WHICH of the two suspend paths (RP-C3 billing-lapse
  -- sweep vs. RP-14 operator suspend) disabled this realm, since that
  -- distinction must survive to the RP-C3 offboard decision (§8.8 excludes
  -- operator suspensions from auto-offboard) — it is never re-derivable
  -- from Org & Membership, which does not track it. NULL when not
  -- currently suspended (RP-10 reactivate clears it); non-NULL only makes
  -- sense while status = 'suspended', but that invariant is enforced by
  -- the writers (RP-14 Suspend / RP-C3 suspendLapsedTenant / RP-10
  -- Reactivate), not a CHECK, to avoid coupling this constraint to every
  -- future status transition.
  CONSTRAINT chk_suspension_source CHECK (suspension_source IS NULL OR suspension_source IN ('billing_lapse', 'operator'))
);

CREATE INDEX idx_tenant_realms_status    ON public.tenant_realms (status)        WHERE deleted_at IS NULL;
CREATE INDEX idx_tenant_realms_trial_exp ON public.tenant_realms (trial_ends_at) WHERE strategy = 'shared' AND status = 'active';
CREATE INDEX idx_tenant_realms_strategy  ON public.tenant_realms (strategy)      WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_tenant_realms_dedicated_name ON public.tenant_realms (realm_name) WHERE strategy = 'dedicated' AND deleted_at IS NULL;
-- RP-C3's offboard-candidate query (billing-lapse suspensions past
-- RETENTION_DAYS) — partial index keeps it cheap despite the table also
-- holding operator-suspended and non-suspended rows.
CREATE INDEX idx_tenant_realms_lapse_offboard ON public.tenant_realms (disabled_at)
  WHERE status = 'suspended' AND suspension_source = 'billing_lapse';

CREATE TRIGGER trg_touch_tenant_realms
    BEFORE UPDATE ON public.tenant_realms
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

ALTER TABLE public.tenant_realms ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_realms FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON public.tenant_realms FROM PUBLIC;
CREATE POLICY tenant_isolation ON public.tenant_realms
    USING      (rls_check_tenant(tenant_id, 'tenant_realms'))
    WITH CHECK (rls_check_tenant(tenant_id, 'tenant_realms'));

-- ═════════════════════════════════════════════════════════════════════════
-- §4.3 keycloak_idp_configs / keycloak_idp_config_validations
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE public.keycloak_idp_configs (
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

CREATE TRIGGER trg_touch_keycloak_idp_configs
    BEFORE UPDATE ON public.keycloak_idp_configs
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

ALTER TABLE public.keycloak_idp_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.keycloak_idp_configs FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON public.keycloak_idp_configs FROM PUBLIC;
CREATE POLICY tenant_isolation ON public.keycloak_idp_configs
    USING      (rls_check_tenant(tenant_id, 'keycloak_idp_configs'))
    WITH CHECK (rls_check_tenant(tenant_id, 'keycloak_idp_configs'));

CREATE TABLE public.keycloak_idp_config_validations (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL,
  idp_alias         text NOT NULL,
  validator_version int  NOT NULL,
  outcome           validation_outcome NOT NULL,
  checks            jsonb NOT NULL,
  acknowledgements  jsonb NOT NULL DEFAULT '[]',
  validated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_idp_validations_tenant ON public.keycloak_idp_config_validations (tenant_id, idp_alias, validated_at DESC);

ALTER TABLE public.keycloak_idp_config_validations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.keycloak_idp_config_validations FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON public.keycloak_idp_config_validations FROM PUBLIC;
CREATE POLICY tenant_isolation ON public.keycloak_idp_config_validations
    USING      (rls_check_tenant(tenant_id, 'keycloak_idp_config_validations'))
    WITH CHECK (rls_check_tenant(tenant_id, 'keycloak_idp_config_validations'));

-- ═════════════════════════════════════════════════════════════════════════
-- §4.4 realm_config_reconciliations / provisioning_operations
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE public.realm_config_reconciliations (
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
CREATE INDEX idx_recon_pending ON public.realm_config_reconciliations (created_at) WHERE status = 'pending';

ALTER TABLE public.realm_config_reconciliations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.realm_config_reconciliations FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON public.realm_config_reconciliations FROM PUBLIC;
CREATE POLICY tenant_isolation ON public.realm_config_reconciliations
    USING      (rls_check_tenant(tenant_id, 'realm_config_reconciliations'))
    WITH CHECK (rls_check_tenant(tenant_id, 'realm_config_reconciliations'));

CREATE TABLE public.provisioning_operations (
  idempotency_key text PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  operation       text NOT NULL,
  status          op_status NOT NULL DEFAULT 'in_progress',
  result          jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  completed_at    timestamptz
);
CREATE INDEX idx_provops_tenant ON public.provisioning_operations (tenant_id, created_at DESC);

-- provisioning_operations is RLS-scoped like the other four tenant-scoped
-- tables even though it is an idempotency ledger, because its `result`
-- payloads can embed tenant data (§4.5).
ALTER TABLE public.provisioning_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.provisioning_operations FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON public.provisioning_operations FROM PUBLIC;
CREATE POLICY tenant_isolation ON public.provisioning_operations
    USING      (rls_check_tenant(tenant_id, 'provisioning_operations'))
    WITH CHECK (rls_check_tenant(tenant_id, 'provisioning_operations'));

-- ═════════════════════════════════════════════════════════════════════════
-- §4.4 pending_audit_entries — durable hand-off of the non-bus
-- TenantIdpConfigChanged audit entry to Audit Log's AL-5 direct-write
-- ingest (HLD §9.4, §10.6). Enqueued in the same RunInTx as the
-- keycloak_idp_configs write it records, so a committed IdP change always
-- carries its audit entry; cmd/server's drainer POSTs each row (id is the
-- AL-5 Idempotency-Key, stable across retries) and deletes it on success.
-- A row Audit permanently rejects (4xx) moves to 'failed' and stays for
-- operator inspection. RLS-scoped because `entry` embeds tenant data (same
-- reasoning as provisioning_operations); the cross-tenant drainer reads
-- it through the BYPASSRLS sysPool.
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE public.pending_audit_entries (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL,
  entry           jsonb NOT NULL,
  status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'failed')),
  attempts        int NOT NULL DEFAULT 0,
  last_error      text,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_pending_audit_due ON public.pending_audit_entries (next_attempt_at) WHERE status = 'pending';

ALTER TABLE public.pending_audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_audit_entries FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON public.pending_audit_entries FROM PUBLIC;
CREATE POLICY tenant_isolation ON public.pending_audit_entries
    USING      (rls_check_tenant(tenant_id, 'pending_audit_entries'))
    WITH CHECK (rls_check_tenant(tenant_id, 'pending_audit_entries'));

-- ═════════════════════════════════════════════════════════════════════════
-- processed_events — service-owned DDL, the consumer dedup ledger (§4.5,
-- §7). Operational and RLS-exempt (not a tenant-query surface).
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE public.processed_events (
  event_id     text NOT NULL,
  consumer     text NOT NULL,       -- 'membership' | 'tenant' | 'billing'
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, consumer)
);
CREATE INDEX idx_processed_events_processed_at ON public.processed_events (processed_at);

-- ═════════════════════════════════════════════════════════════════════════
-- exporter_watermarks — one row per cmd/server metric exporter that scrapes
-- an append-only table on a high-water-mark basis instead of a fixed
-- trailing window (currently just rls_violations_total). Persisting the
-- mark in Postgres, shared by every server replica and surviving pod
-- restarts, is what makes the exporter correct under both: without it, a
-- restart mid-window re-counts violations the dying pod already tallied,
-- and with more than one HPA replica (minReplicas: 2) every replica
-- independently re-counts the same rows all the time, not just around a
-- restart. RLSViolationCounts (internal/adapter/outbound/postgres) takes
-- SELECT ... FOR UPDATE on this table's row, so concurrent replicas
-- serialize on it — a second replica's tick blocks until the first
-- commits its advanced mark, then sees an already-drained window.
-- Operational and RLS-exempt (not a tenant-query surface), like
-- processed_events above.
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE public.exporter_watermarks (
  exporter_name     text        PRIMARY KEY,
  last_processed_at timestamptz NOT NULL
);

-- ═════════════════════════════════════════════════════════════════════════
-- Roles & grants. Three DB roles in play (§4.5, §10.1):
--   • realm_provisioner_app       — runtime app. Does NOT hold BYPASSRLS.
--   • realm_provisioner_migrator  — migrations + reconciler. Holds BYPASSRLS.
--   • admin_readonly               — cross-tenant compliance/support SELECTs.
--
-- In production the roles are provisioned by Terraform (owns CREATE ROLE);
-- this migration attaches grants and idempotently re-asserts BYPASSRLS. In
-- dev the migrator user typically lacks CREATEROLE, so we skip creation
-- gracefully and emit a NOTICE. Idempotent + safe to re-run in every env.
-- ═════════════════════════════════════════════════════════════════════════
DO $$
DECLARE
    v_can_create boolean;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            ALTER ROLE admin_readonly BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'admin_readonly exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh (fine in dev).';
        END;
    ELSE
        SELECT rolcreaterole OR rolsuper INTO v_can_create FROM pg_roles WHERE rolname = current_user;
        IF v_can_create THEN
            CREATE ROLE admin_readonly NOLOGIN BYPASSRLS;
        ELSE
            RAISE NOTICE 'admin_readonly role missing and current user (%) lacks CREATEROLE — skipping. Provisioned by Terraform in prod.', current_user;
        END IF;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            GRANT SELECT ON public.tenant_realms, public.keycloak_idp_configs,
                public.keycloak_idp_config_validations, public.realm_config_reconciliations,
                public.provisioning_operations, public.pending_audit_entries,
                public.rls_violation_log, public.processed_events
                TO admin_readonly;
            EXECUTE $c$COMMENT ON ROLE admin_readonly IS
                'Cross-tenant compliance/support reader. BYPASSRLS + SELECT only.'$c$;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grants to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'realm_provisioner_migrator') THEN
        BEGIN
            ALTER ROLE realm_provisioner_migrator BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'realm_provisioner_migrator exists but current user cannot ALTER ROLE.';
        END;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'realm_provisioner_app' AND rolbypassrls) THEN
        BEGIN
            ALTER ROLE realm_provisioner_app NOBYPASSRLS;
            RAISE NOTICE 'Stripped BYPASSRLS from realm_provisioner_app (RLS invariant restored).';
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE WARNING 'realm_provisioner_app has BYPASSRLS but current user cannot ALTER ROLE — VIOLATION requires operator action.';
        END;
    END IF;
END$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'realm_provisioner_app') THEN
        RAISE NOTICE 'realm_provisioner_app role missing — skipping grants. Terraform provisions this in prod.';
        RETURN;
    END IF;

    BEGIN
        GRANT EXECUTE ON FUNCTION public.app_tenant_id()                     TO realm_provisioner_app;
        GRANT EXECUTE ON FUNCTION public.log_rls_violation(text, uuid, text) TO realm_provisioner_app;
        GRANT EXECUTE ON FUNCTION public.rls_check_tenant(uuid, text)        TO realm_provisioner_app;

        GRANT SELECT, INSERT, UPDATE, DELETE ON public.tenant_realms                    TO realm_provisioner_app;
        GRANT SELECT, INSERT, UPDATE, DELETE ON public.keycloak_idp_configs              TO realm_provisioner_app;
        GRANT SELECT, INSERT                 ON public.keycloak_idp_config_validations   TO realm_provisioner_app;
        GRANT SELECT, INSERT, UPDATE          ON public.realm_config_reconciliations      TO realm_provisioner_app;
        GRANT SELECT, INSERT, UPDATE          ON public.provisioning_operations           TO realm_provisioner_app;
        GRANT SELECT, INSERT                 ON public.pending_audit_entries             TO realm_provisioner_app;
        GRANT SELECT, INSERT, DELETE          ON public.processed_events                  TO realm_provisioner_app;

        -- outbox_events: created by platform-events; guarded IF EXISTS
        -- because platform-events owns the migration and this domain
        -- migration only customizes it (§4.5, §19).
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_events TO realm_provisioner_app;
        END IF;
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE NOTICE 'Grants to realm_provisioner_app skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
END$$;

-- ═════════════════════════════════════════════════════════════════════════
-- outbox_events customization (§4.5, §19). platform-events'
-- outbox.ApplySchema creates outbox_events with a JSONB payload column
-- BEFORE this migration runs; pgx encodes []byte as bytea hex in PgBouncer
-- SimpleProtocol mode, which is invalid for jsonb — text accepts the raw
-- bytes as-is, and the outbox runner JSON-unmarshals it back either way.
--
-- FOOTGUN if this "runs once, against an empty DB, before any traffic"
-- invariant is ever violated (§19 — this is the only consolidated
-- migration this service has ever shipped): a USING-clause ALTER COLUMN
-- TYPE takes an ACCESS EXCLUSIVE lock and rewrites the entire table. On
-- the intended path that's instant (the table is empty). Do not reuse this
-- file as a template for re-running against a populated outbox_events
-- (e.g. a partial DR restore, or forcing a re-run by deleting a
-- schema_migrations row) — that would lock out every in-flight outbox
-- write for as long as the rewrite takes. The four non-CONCURRENTLY
-- CREATE INDEX statements on tenant_realms below carry the same caveat at
-- lower severity (a write-blocking SHARE lock, not a full rewrite).
-- ═════════════════════════════════════════════════════════════════════════
ALTER TABLE outbox_events ALTER COLUMN payload TYPE text USING payload::text;

-- pgx SimpleProtocol encodes []byte as bytea hex (\x...) even for text
-- columns. This trigger decodes it back to UTF-8 text on every INSERT so
-- the outbox runner can JSON-unmarshal the payload without errors.
CREATE OR REPLACE FUNCTION outbox_normalize_payload()
RETURNS trigger AS $$
BEGIN
  IF left(NEW.payload, 2) = '\x' THEN
    NEW.payload = convert_from(decode(substring(NEW.payload FROM 3), 'hex'), 'UTF8');
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_outbox_normalize_payload
BEFORE INSERT ON outbox_events
FOR EACH ROW EXECUTE FUNCTION outbox_normalize_payload();
