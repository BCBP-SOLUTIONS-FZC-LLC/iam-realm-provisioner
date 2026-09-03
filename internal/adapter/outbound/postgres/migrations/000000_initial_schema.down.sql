-- ── outbox_events customization (reverse first — added last in up.sql) ──
DROP TRIGGER IF EXISTS trg_outbox_normalize_payload ON outbox_events;
DROP FUNCTION IF EXISTS outbox_normalize_payload();
ALTER TABLE outbox_events ALTER COLUMN payload TYPE jsonb USING payload::jsonb;

REVOKE ALL ON public.processed_events FROM realm_provisioner_app;
REVOKE ALL ON public.provisioning_operations FROM realm_provisioner_app;
REVOKE ALL ON public.realm_config_reconciliations FROM realm_provisioner_app;
REVOKE ALL ON public.keycloak_idp_config_validations FROM realm_provisioner_app;
REVOKE ALL ON public.keycloak_idp_configs FROM realm_provisioner_app;
REVOKE ALL ON public.tenant_realms FROM realm_provisioner_app;

DROP TABLE IF EXISTS public.processed_events;
DROP TABLE IF EXISTS public.exporter_watermarks;

DROP TABLE IF EXISTS public.provisioning_operations;
DROP TABLE IF EXISTS public.realm_config_reconciliations;

DROP TABLE IF EXISTS public.keycloak_idp_config_validations;
DROP TABLE IF EXISTS public.keycloak_idp_configs;

DROP TRIGGER IF EXISTS trg_touch_tenant_realms ON public.tenant_realms;
DROP TABLE IF EXISTS public.tenant_realms;

DROP FUNCTION IF EXISTS public.rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS public.log_rls_violation(text, uuid, text);
DROP TABLE IF EXISTS public.rls_violation_log;
DROP FUNCTION IF EXISTS public.touch_row();
DROP FUNCTION IF EXISTS public.app_tenant_id();

DROP TYPE IF EXISTS op_status;
DROP TYPE IF EXISTS recon_status;
DROP TYPE IF EXISTS validation_outcome;
DROP TYPE IF EXISTS idp_config_status;
DROP TYPE IF EXISTS idp_protocol;
DROP TYPE IF EXISTS realm_status;
DROP TYPE IF EXISTS realm_strategy;
