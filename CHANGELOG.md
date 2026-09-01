# Changelog

All notable changes to this service are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning aligned with SemVer.

This service has never been deployed to any environment — there is no released version yet, so everything lives under `[Unreleased]`. Because of that, this file tracks only what is currently true and what is still outstanding, not a phase-by-phase build history; the full development diary is available via `git log` if it's ever needed.

## [Unreleased]

### Added

Initial build of the Realm Provisioner Service — the single writer to the Keycloak Admin API and system of record for tenant identity lifecycle (LLD `docs/lld/iam-lld-realm-provisioner-service.md`, v1.0 Approved).

- **HTTP API — 16 endpoints (RP-1..RP-16),** all under `/api/v1/internal`: `provision-trial`, `provision-direct`, `:id/convert`, `:id/revert-conversion`, `:id/users` (invite), `:id/idp` (PUT), `:id/idp/:alias` (DELETE), `:id/realm-config` (PATCH), `:id/users/:uid/mfa-reset`, `:id/reactivate`, `:id/realm` (GET), `:id/expire-trial`, `audit/idp-region-portability`, `:id/suspend`, `:id/users/:keycloak_user_id` (DELETE), `:id/users/:keycloak_user_id/revoke-sessions`.
- **Keycloak admin adapter** (`internal/adapter/outbound/keycloak/`) — the sole implementation of the `KeycloakAdmin` port (RP-INV-1 single-writer invariant, enforced in CI). Realm creation from template, OIDC client registration, user creation/migration/deletion, session revocation, MFA realm-role sync, IdP create/update/delete, realm enable/disable/export.
- **OpenBao adapter** (`internal/adapter/outbound/openbao/`) — Kubernetes-auth-backed KV v2 client for SAML/OIDC IdP secrets at the frozen path `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`. No IdP secret material is ever persisted to Postgres.
- **Postgres schema** (`internal/adapter/outbound/postgres/migrations/`) — `tenant_realms`, `keycloak_idp_configs`, `keycloak_idp_config_validations`, `realm_config_reconciliations`, `provisioning_operations` (5 tenant-scoped tables, `FORCE ROW LEVEL SECURITY`); `outbox_events` (via `platform-events`' `outbox.ApplySchema`) and `processed_events` (RLS-exempt).
- **Transactional outbox + 9 produced events** on `iam-tenant-events`: `TrialTenantProvisioned`, `TenantRealmReady`, `TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`, `TenantOffboarded`, `TenantReactivated`.
- **3 SQS consumers**: `membership-realm-q` (`DepartmentMembershipGranted`, `DepartmentMembershipLevelChanged`, `TenantRoleGranted`, `TenantRoleRevoked` → MFA realm-role sync), `tenant-realm-q` (`TrialStarted` → trial timer record), `billing-realm-q` (`TenantReactivated` → billing-driven reactivation). Deduped via `processed_events`.
- **6 reconciler jobs** (`cmd/reconciler`, dispatched via `--job=<name>`): `trial-expiry` and `trial-hard-delete` (RP-C1/RP-C2), `subscription-lapse` (RP-C3), `realm-config-reconcile` (RP-C4), plus `outbox-prune` and `processed-events-prune` retention hygiene jobs.
- **Idempotency ledger** (`provisioning_operations`, keyed by the caller's `Idempotency-Key` header) and Keycloak-effect-first-then-local-commit ordering discipline (§9.1) with compensating teardown on partial multi-step failures (§9.3).
- **CI pipeline** (`.github/workflows/`) — `validate-test.yml` (unit/postgres-RLS/integration tests with race detector + coverage gate + arch-lint + e2e), `validate-quality.yml` (fmt/tidy/vet/lint/vuln-check/Dockerfile digest pinning), `ci.yml` (orchestration + single-image Docker build carrying both binaries + Trivy scan + Cosign signing), `changelog-check.yml`. Includes an AsyncAPI-vs-`internal/eventschema` drift test (`internal/eventschema/asyncapi_drift_test.go`) as this service's CI-enforceable substitute for a live Glue-registry comparison.
- **Deploy manifests** (`deploy/`) — Helm chart (Deployment + CronJobs sharing one image), AWS IAM policy for SNS/SQS/Glue/S3/KMS, OpenBao policy + Kubernetes-auth role binding, Prometheus alert rules for the `iam_realm_provisioner_*` metric family, Keycloak realm template.

### Known gaps

- **RP-C3 subscription-lapse job** — the frozen LLD (§8.9/§16) specifies that this job reads subscription status from Org & Membership, but does not name the concrete O&M endpoint that serves it (`internal/core/port/subscription_status_reader.go` documents this as an open item). The job is implemented against the `SubscriptionStatusReader` port with no adapter wired in `cmd/reconciler/main.go` yet — coordinate with the O&M team before wiring a concrete HTTP client.
- **OpenBao integration** has been structurally reviewed and unit-tested, but never exercised against a live OpenBao instance — the Kubernetes ServiceAccount token file the auth flow depends on only exists inside a real cluster. RP-6/RP-7 will return `500` in any local-dev environment that attempts an IdP secret write/delete without a real OpenBao + Kubernetes auth binding.
- `failing_tenants` in the RP-13 audit response serializes as `null` rather than `[]` when the audit finds zero failing tenants — cosmetic, not yet fixed.
