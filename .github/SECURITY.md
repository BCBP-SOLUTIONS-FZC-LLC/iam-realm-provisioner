# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| 1.x     | Active |

Older major versions are not patched. Deploy the latest 1.x image.

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Email: vijay@bcbpsolutions.com
Subject: `[iam-realm-provisioner] Security vulnerability`

Include in your report:
- Description of the vulnerability and the affected component (HTTP endpoint, Keycloak admin adapter, OpenBao adapter, SQS consumer, reconciler job, RLS policy, etc.)
- Steps to reproduce
- Potential impact (Keycloak admin credential exposure, IdP secret leakage, RLS bypass, tenant realm cross-contamination, privilege escalation, DoS, etc.)
- Suggested fix or patch (if any)

### Response timeline

| Step | Target |
|------|--------|
| Initial acknowledgement | 48 hours |
| Severity assessment | 5 business days |
| Patch release (critical/high) | 14 days |
| Public disclosure | After patch ships |

We follow responsible disclosure. Reporters will be credited in release notes unless anonymity is requested.

## Scope

Areas of particular sensitivity in this service:

- **RP-INV-1 (single Keycloak writer)** — every mutating call to the Keycloak Admin API goes through the `KeycloakAdmin` port's single gocloak-backed implementation (`internal/adapter/outbound/keycloak/`). A second code path calling Keycloak directly would bypass audit logging, idempotency, and the compensating-teardown discipline this service relies on.
- **No secret material in Postgres** — realm passwords/seeds live only in Keycloak; SAML/OIDC IdP client secrets live only in OpenBao at `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`. Any code path that would persist a credential to `tenant_realms`/`idp_configs` is a defect.
- **Row-Level Security (RLS)** — tenant isolation enforced at the Postgres layer with `FORCE ROW LEVEL SECURITY` on all 5 tenant-scoped tables. `app.tenant_id` must always be bound transaction-locally (`SET LOCAL` via pgcommon's `GUCSetFromContext`/`WithGUCSet`) — a session-scoped `SET` would leak across tenants under PgBouncer connection reuse (RLS-6).
- **Idempotency / replay safety** — the `provisioning_operations` ledger and `processed_events` dedup table exist specifically to make retried HTTP calls and redelivered SQS messages safe; a bypass could double-provision a realm, double-migrate a user, or re-fire a produced event.
- **Keycloak-effect-first-then-local-commit ordering** — a Keycloak mutation must succeed before its corresponding local Postgres row commits; violating this ordering risks phantom local rows describing Keycloak state that was never actually created.
- **Outbox event payloads** — validated against embedded JSON Schemas (`internal/eventschema/`) before insert; a bypass could propagate malformed tenant-lifecycle events to every downstream consumer of `iam.tenant.events`.
