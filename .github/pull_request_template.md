
## Description
Provide a clear description of the changes.

---

## Type of Change
- [ ] Bug fix
- [ ] New feature
- [ ] Refactor
- [ ] Documentation
- [ ] Test
- [ ] Breaking change
- [ ] New migration
- [ ] Event contract change (asyncapi.yaml or eventschema/)

---

## Testing
- [ ] Unit tests added/updated (`make test-unit`)
- [ ] Postgres / RLS integration tests added/updated (`make test-postgres`) — every new/changed policy or query against a tenant-scoped table covered by the §17.5 RLS matrix
- [ ] Integration tests added/updated (`make test-integration`)
- [ ] All tests passing with race detector (`make race`)
- [ ] Manual testing performed (if required)

---

## Checklist

### Code Quality
- [ ] Code is properly formatted (`make fmt-check`)
- [ ] Linting passed (`make lint`)
- [ ] Vet passed (`make vet`)
- [ ] Architecture layering passed (`make arch-lint`) — includes RP-INV-1 (single Keycloak writer) and RLS-6 (no session-scoped tenant GUC) checks
- [ ] No debug logs / commented-out code
- [ ] No secrets or DSNs hardcoded

### Event Contract
- [ ] AsyncAPI spec updated if a new event type or payload field was added (`api/asyncapi.yaml`)
- [ ] Event schema JSON files updated to match (`internal/eventschema/*.json`)
- [ ] `internal/eventschema/asyncapi_drift_test.go` passes — the AsyncAPI channel's message list and `eventschema.ByEventType`'s keys stay in lockstep
- [ ] Frozen §25 event/field names were not renamed

### RP-INV-1 / Secret Handling
*Complete only when a Keycloak or OpenBao code path was added or changed.*
- [ ] No new code path calls the Keycloak Admin API outside `internal/adapter/outbound/keycloak/` (CI's gocloak-isolation grep enforces this, but double-check)
- [ ] No secret material (passwords, seeds, SAML/OIDC client secrets) is persisted to Postgres
- [ ] Any new OpenBao path stays under the existing `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>` scope — no widened mount or path pattern

### RLS / Tenant Isolation
*Complete only when a new query, table, or GUC-binding path was added.*
- [ ] Every new query against a tenant-scoped table runs inside a transaction with `app.tenant_id` bound via `pgcommon`'s `SET LOCAL` semantics — never a session-scoped `SET app.tenant_id` (CI's forbidden-GUC grep enforces this, but double-check any new call path)
- [ ] New repository methods that intentionally read/write cross-tenant (reconciler sweep finders) are documented as requiring the BYPASSRLS pool, not the RLS-scoped one
- [ ] §17.5 RLS matrix (Cases 1–5) still passes on any table this PR touches

### Idempotency / Replay Safety
- [ ] New mutating endpoints honor the `Idempotency-Key` header via `provisioning_operations`
- [ ] New consumed-event handling is deduped via `processed_events`
- [ ] Keycloak-effect-first-then-local-commit ordering preserved (§9.1) for any new multi-step provisioning flow

### Database / Migrations
- [ ] New migrations have matching `.up.sql` and `.down.sql`
- [ ] Down migration correctly reverses the up migration
- [ ] RLS policies tested with `FORCE ROW LEVEL SECURITY` (`make test-postgres`)
- [ ] Migration tested against PgBouncer transaction-pooling mode (`PGBouncerMode: true`)

### Security
- [ ] No secrets or DSNs hardcoded
- [ ] GUC values are not logged (no credential leakage in slow-query output)
- [ ] New config fields documented in README's env-vars table

### Documentation
- [ ] README updated (if public API, env vars, or config changed)
- [ ] ARCHITECTURE.md updated (if layering, flows, key invariants, or a new session-specific decision applies)
- [ ] CHANGELOG.md updated

---

## Related Issue
Closes #<issue-id>

---

## Deployment Notes
Mention anything important for operators upgrading (migration steps, new required env vars, config changes, breaking event schema changes).
