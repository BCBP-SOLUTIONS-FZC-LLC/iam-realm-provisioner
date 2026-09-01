# Request Flows & Concurrency

## Cross-Cutting Discipline: Keycloak-Effect-First, Then Local Commit (§9.1)

Every provisioning/lifecycle mutation in this service follows the same shape, visible in every method in `provisioning_service.go` and `lifecycle_service.go`:

1. `idempotency.Begin(ctx, key, tenantID, operation)` — if an existing row is found, either replay its `succeeded` result or return `409 operation_in_progress`.
2. Call Keycloak **first**, entirely **outside** any Postgres transaction.
3. Only on Keycloak success, open `tx.RunInTx(...)` and, in one transaction: update/insert the local row (`tenant_realms`, `keycloak_idp_configs`, etc.) **and** `EnqueueCtx` the produced event(s) via the transactional outbox.
4. On any failure at step 2, call `failIdempotency` (best-effort, logged not propagated) and return the error — no local row is ever written.
5. On success, marshal the result and `idempotency.Complete(ctx, key, body)`.

This ordering means a failed Keycloak call never leaves a phantom local row, and a failed local commit (rare — same-process Postgres write) never claims a Keycloak-side effect that didn't happen, since the local write always follows the Keycloak effect in program order, never the reverse.

## Compensating Teardown (§9.3)

Multi-step Keycloak sequences (`ProvisionDirect`, `Convert`) call `bestEffortDeleteRealm` (→ `KeycloakAdmin.DeleteRealmIfExists`, itself idempotent) if a **later** step in the same sequence fails, tearing down the realm created by an **earlier** step in that same call:

- `ProvisionDirect`: realm created → OIDC client registration fails → delete the realm. Realm created → clients registered → admin user creation fails → delete the realm.
- `Convert`: realm created → OIDC registration fails → delete the realm (`ErrConversionFailed`). Realm created → clients registered → listing trial users fails → delete the realm. Realm created → clients registered → users listed → a `MigrateUser` call fails partway through the loop → delete the (new) dedicated realm — **the shared realm is untouched**, and any users already migrated by earlier loop iterations stay migrated (`MigrateUser`'s own semantics delete the source as part of the copy, so a retry with the same `Idempotency-Key` re-lists whatever remains in the shared realm on the next attempt and continues from there — the operation is naturally resumable, not just retryable-from-scratch).

Teardown failures are logged, not escalated — an orphaned partially-created realm is caught by manual cleanup or a future reconciler pass, never by blocking the caller's error response.

## RP-1: Trial Provisioning

```
POST /tenants/provision-trial {tenant_id, tenant_name, tenant_slug, admin_email, admin_name, hashed_password_seed?, plan}
  idempotency.Begin(key, tenant_id, "provision_trial")
    found + succeeded -> replay
    found + other     -> 409 operation_in_progress
  Keycloak.CreateTrialUser(email, full_name, tenant_id, seed, email_verified=false)
    -> stamps tenant_id attribute on the user, triggers VERIFY_EMAIL (fail -> failIdempotency, return err)
  RunInTx:
    tenant_realms.Upsert{tenant_id, realm_name:"trial", strategy:"shared", status:"active"}
    outbox.Enqueue(TrialTenantProvisioned{tenant_id, realm:"trial", keycloak_user_id})
  idempotency.Complete(key, marshaled result)
  -> 201 {tenant_id, realm:"trial", strategy:"shared", keycloak_user_id, email_verification_sent:true}
```

## RP-2: Direct-Paid Provisioning

```
POST /tenants/provision-direct {..., billing_subscription_id}
  realmName := "tenant-" + tenant_slug
  shard := selectShard()  -- least-loaded of the configured shardIDs, ties broken lexicographically for
                             deterministic behavior under concurrent placements
  Keycloak.CreateRealmFromTemplate(realmName, "v1")           -- fail -> failIdempotency, return err (no teardown needed, nothing created yet)
  Keycloak.RegisterOIDCClients(realmName, tenant_slug)        -- fail -> bestEffortDeleteRealm(realmName), failIdempotency, return err
  Keycloak.CreateInvitedUser(realmName, {..., RequiredActions:["VERIFY_EMAIL"]}) -- fail -> bestEffortDeleteRealm, failIdempotency, return err
  RunInTx:
    tenant_realms.Upsert{tenant_id, realm_name:realmName, strategy:"dedicated", keycloak_shard:shard, status:"active", converted_at:now()}
    outbox.Enqueue(TenantRealmReady{tenant_id, realm, keycloak_shard, oidc_clients})
    outbox.Enqueue(DirectPaidSignup{tenant_id, realm, direct_signup:true})
    outbox.Enqueue(TenantConverted{tenant_id, realm, plan, billing_subscription_id?, users_migrated:0, direct_signup:true})
  -> 201 {tenant_id, realm, strategy:"dedicated", keycloak_shard, oidc_clients}
```

Note `selectShard` reads `CountsByShard` from the **local mirror** of `tenant_realms` (this service is the single writer to Keycloak, so its own local count is authoritative without a live Keycloak query, §9.5).

## RP-3: Trial → Dedicated Conversion (with user migration)

```
POST /tenants/:id/convert {plan, billing_subscription_id, payment_method_id (accepted, never stored/used — PCI/Billing domain)}
  current := tenant_realms.FindByTenantID(tenant_id)
  current.Strategy != "shared" -> 409 realm_already_dedicated
  slug := tenant_id[:8]   -- Convert's request body carries no slug (§5.2); this service persists no
                             tenant_slug of its own, so a stable name is derived from the tenant id itself
  realmName := "tenant-" + slug
  shard := selectShard()
  Keycloak.CreateRealmFromTemplate(realmName, "v1")                    -- fail -> failIdempotency, return err
  Keycloak.RegisterOIDCClients(realmName, slug)                        -- fail -> bestEffortDeleteRealm, 502 conversion_failed
  sourceUsers := Keycloak.ListUsersByTenant(current.RealmName, tenant_id) -- fail -> bestEffortDeleteRealm, 502 conversion_failed
  for each uid in sourceUsers:
    reenroll := Keycloak.MigrateUser(current.RealmName, realmName, uid)  -- fail -> bestEffortDeleteRealm, 502 conversion_failed
                                                                            (partial migrations in this loop stay migrated —
                                                                             MigrateUser already deleted them from the source)
    reenroll -> webauthnReenroll++
  Keycloak.ForceLogoutRealm(current.RealmName)   -- best-effort, logged WARN on failure, non-fatal (sessions expire naturally)
  RunInTx:
    current.{Strategy:"dedicated", RealmName:realmName, KeycloakShard:shard, Status:"active", ConvertedAt:now()}
    tenant_realms.Update(current)
    outbox.Enqueue(TenantRealmReady{...})
    outbox.Enqueue(TenantConverted{..., users_migrated:len(sourceUsers), direct_signup:false})
  -> 200 {tenant_id, realm, strategy:"dedicated", keycloak_shard, oidc_clients, users_migrated, webauthn_reenroll_required, shared_sessions_terminated:true}
```

`MigrateUser` (in `internal/adapter/outbound/keycloak/`, not itself part of this service-layer flow but the mechanism the loop above calls per user) recreates the user in the target realm preserving the Keycloak `sub` (via the `partialImport` admin endpoint — the standard create-user endpoint ignores a caller-specified ID), copies the TOTP credential, and flags `WEBAUTHN_REGISTER` when the source held a WebAuthn credential (WebAuthn credentials cannot migrate across realms — the browser-bound authenticator is tied to the origin/realm it enrolled against). Because Keycloak's user ID is a **global** primary key across every realm in the deployment, the source user is deleted from the source realm *before* being recreated with the same ID in the target realm — a same-ID user cannot exist in two realms simultaneously.

## RP-4: Revert Conversion

```
POST /tenants/:id/revert-conversion
  current.Strategy != "dedicated" -> 400 invalid_request
  now - current.ConvertedAt > revertWindowDays*24h -> 409 revert_window_expired {converted_at}
  sourceUsers := Keycloak.ListUsersByTenant(current.RealmName, tenant_id)  -- fail -> 502 conversion_failed
  for each uid: Keycloak.MigrateUser(dedicatedRealm, sharedRealm, uid)     -- fail -> 502 conversion_failed
  Keycloak.DeleteRealmIfExists(dedicatedRealm)
  RunInTx:
    current.{Strategy:"shared", RealmName:sharedRealm, KeycloakShard:nil, ConvertedAt:nil, TrialEndsAt:now()+newTrialDurationDays, Status:"active"}
    tenant_realms.Update(current)
    outbox.Enqueue(TrialReactivated{tenant_id, new_trial_ends_at})
  -> 200 {tenant_id, realm:"trial", strategy:"shared"}
```

## RP-6/RP-7: IdP Configuration Under the §11.6 Gate

```
PUT /tenants/:id/idp {idp_alias, protocol, display_name, config{non-secret}, secret, acknowledgements[]}
  realm := tenant_realms.FindByTenantID(tenant_id)
  realm.Strategy != "dedicated" -> 409 idp_requires_dedicated_realm
  validation := ValidateIdpConfig(protocol, config, acknowledgements)  -- §11.6 checklist, see below
  keycloak_idp_config_validations.Insert(tenant_id, idp_alias, ValidatorVersion, outcome, per-check results, acks)
    (best-effort — a failure to record is logged WARN, not propagated)
  outcome == failing        -> 400 idp_region_portability_failed {failed_checks, warnings}
  outcome == warnings_only  -> 422 idp_warnings_unacknowledged {warnings}
  -- else (pass, or warnings_only fully acknowledged):
  if secret != "": OpenBao.WriteIdpSecret(tenant_id, idp_alias, {secret})   -- fail -> return err (nothing written to Keycloak/Postgres yet)
  Keycloak.CreateOrUpdateIdp(realm.RealmName, {alias, protocol, display_name, enabled:true, config, secret})
  keycloak_idp_configs.Upsert{tenant_id, idp_alias, protocol, display_name, enabled:true, config, status:"active"}
  -> 200 {idp_alias, status:"active"}

DELETE /tenants/:id/idp/:alias
  realm := tenant_realms.FindByTenantID(tenant_id)
  Keycloak.DeleteIdp(realm.RealmName, alias)
  OpenBao.DeleteIdpSecret(tenant_id, alias)
  keycloak_idp_configs.Delete(tenant_id, alias)
  -> 204
```

**§11.6 Region-Agnostic IdP Configuration Checklist** (`idp_validator.go`): checks every SAML/OIDC config field against a fixed list of AWS region substrings (`ap-south-1`, `us-east-1`, etc.) and against the one platform-wide region-neutral auth host (`auth.bcbpsolutions.com`). CRITICAL checks — SSO/SLO URL region, customer signing cert present, ACS URL points at the neutral host, entity ID region, OIDC issuer/redirect/audience region — **block** with `400`. WARN checks — SAML `NameIDFormat != "transient"`, attribute-mapper URNs region-free, strong signature algorithm (`RSA-SHA(256|384|512)`), OIDC PKCE `S256` — require a recorded acknowledgement (by check ID) or return `422`. RP-13's whole-estate audit re-runs the same `ValidateIdpConfig` function against every stored config's non-secret `Config` (never re-reading secrets from OpenBao) to compute pass/warnings/failing counts.

## RP-8 & RP-C4: Realm Config, Local-Intent-First (RP-INV-6)

```
PATCH /tenants/:id/realm-config {setting_key, desired_value}
  !IsAllowedSettingKey(setting_key) -> 400 unsupported_realm_setting {allowed:["local_accounts_enabled","idp:<alias>"]}
  realm := tenant_realms.FindByTenantID(tenant_id)
  Keycloak.ApplyRealmConfig(realm.RealmName, setting_key, desired_value)
    success -> 200 {applied:true}
    failure -> realm_config_reconciliations.Insert{tenant_id, setting_key, desired_value, status:"pending"}
               -> 200 {applied:false, reconciling:true}   -- NOT an error response; the caller's intent is durably recorded
```
RP-C4 (`*/10 * * * *`) sweeps every `status='pending'` row: re-resolves the realm, re-attempts `ApplyRealmConfig`; success → `MarkApplied`; failure → `MarkFailed(lastErr)` and the row stays pending for the next tick. This is the one place in the service where a client-facing 200 response does **not** mean the Keycloak-side effect has actually landed yet — by design, since blocking the caller on Keycloak availability for a config patch would be worse than converging asynchronously.

## RP-9 & membership-realm-q: MFA

```
POST /tenants/:id/users/:keycloak_user_id/mfa-reset
  realm := tenant_realms.FindByTenantID(tenant_id)
  requiresMFA := Keycloak.HasRequiresMFA(realm.RealmName, keycloak_user_id)
  Keycloak.ResetMFA(realm.RealmName, keycloak_user_id, requiresMFA)  -- deletes every OTP/WebAuthn credential;
                                                                        re-adds CONFIGURE_TOTP iff requiresMFA
  -> 204   (fail-closed: any error surfaces to the caller as-is; no local state, no reconciler)
```

`membership-realm-q` consumer → `MfaService.SyncRequiresMfa(tenant_id, user_id, grant bool)` — a **reconcile**, not an apply-a-delta, so redelivery/out-of-order messages converge automatically:
- **Grant** (`DepartmentMembershipGranted`/`DepartmentMembershipLevelChanged` at reviewer/approver level, `TenantRoleGranted`): `AssignRequiresMFA` — assign-if-absent, unconditional.
- **Revoke** (`TenantRoleRevoked` **only**): first calls `OrgMembershipClient.HasResidualMFARequirement(tenant_id, user_id)` — O&M's I-8-backed check for whether the user still holds some *other* MFA-requiring role/level, since this service holds no membership state of its own. Only if that returns `false` does it call `RevokeRequiresMFA` (revoke-if-present).

## RP-10: Reactivate

```
POST /tenants/:id/reactivate {source, reason?, new_trial_ends_at?}
  row := tenant_realms.FindByTenantID(tenant_id)
  row.IsOffboarded() -> 409 tenant_offboarded  (terminal, RP-INV-5)
  row.Status == "active" -> idempotent no-op, complete idempotency, return current status
  row.Strategy == "shared"    -> Keycloak.EnableUsersInRealm(realm, tenant_id)
  row.Strategy == "dedicated" -> Keycloak.EnableRealm(realm)
  RunInTx:
    row.{Status:"active", DisabledAt:nil, TrialEndsAt:new_trial_ends_at? if shared}
    tenant_realms.Update(row)
    row.Strategy=="shared" -> outbox.Enqueue(TrialReactivated{...})
    else                   -> outbox.Enqueue(TenantReactivated{tenant_id, realm, source, reason})
  -> 200 {tenant_id, status:"active"}
```
One endpoint reverses **both** a billing-lapse suspension and an RP-14 operator suspension — `source`/`reason` on the request only drive which event fires, not the mechanics.

## RP-12 & RP-C1: Trial Expiry

`ExpireTrial` (RP-12) is the idempotent per-tenant effect: `DisableUsersInRealm` → `RunInTx{Status:"disabled", DisabledAt:now()}` + `outbox.Enqueue(TrialExpired{tenant_id, trial_ends_at, grace_ends_at})`. RP-C1 (`0 2 * * *`) sweeps `tenant_realms WHERE strategy='shared' AND status='active' AND trial_ends_at < now()` (`ListExpiredTrials`, backed by `idx_tenant_realms_trial_exp`) — **never queries O&M** — and calls the same idempotent effect per row, keyed `"trial-expiry:<tenant_id>:<trial_ends_at>"` so a changed `trial_ends_at` (e.g. from a reactivation) naturally produces a fresh idempotency key rather than colliding with a stale one.

## RP-14: Operator Suspend

`Suspend` — `RequireOperatorRole()` at the route level. Dedicated realm → `Keycloak.DisableRealm`; shared-strategy tenant → `Keycloak.DisableUsersInRealm` **only that tenant's users, never the shared realm itself** (RP-INV-2 — the shared realm hosts every trial tenant). Emits `TenantSuspended{source:"operator"}` — distinguishable downstream from RP-C3's `source:"billing_lapse"` suspension.

## RP-15/RP-16: Idempotent User Operations

Both resolve the realm from `tenant_realms` then delegate straight to `KeycloakAdmin.DeleteUser`/`RevokeUserSessions` — no local state, no idempotency ledger entry (the underlying Keycloak operations are themselves naturally idempotent: deleting an absent user succeeds; revoking sessions for a user with none active succeeds).

## RP-C2: Trial Hard Delete

`0 3 * * *`. Runs entirely against the `SysPool` (BYPASSRLS) with raw SQL (`runInTxWithSysPool`), not through the `TenantRealmRepository` port — selects `tenant_realms WHERE strategy='shared' AND status='disabled' AND disabled_at < now() - TRIAL_GRACE_DAYS`, calls `Keycloak.HardDeleteUsersInRealm` per row, then `UPDATE tenant_realms SET status='deleted', deleted_at=now()`. No event is emitted — O&M runs its own independent cleanup cron for its `tenants` row; the two services do not coordinate this step.

## RP-C3: Subscription Lapse (Suspend → Offboard)

`0 2 * * *`. Reads every dedicated tenant's subscription state via `jctx.SubscriptionStatus.ListDedicatedTenantSubscriptions` — **see the open item in `operations.md`: this reader is currently `nil` in `cmd/reconciler/main.go`, so the job logs a WARN and returns immediately, a no-op, until O&M's endpoint contract is settled.** Once wired, per tenant:
- `status=="cancelled"` AND `now - cancelled_at >= CANCELLED_GRACE_DAYS` → `suspendLapsedTenant`: `DisableRealm` → `RunInTx{Status:"suspended", DisabledAt:now()}` + `outbox.Enqueue(TenantSuspended{source:"billing_lapse", cancelled_at})`.
- `status=="suspended" AND suspension_source=="billing_lapse"` AND `now - suspended_at >= RETENTION_DAYS` → `offboardTenant`: `ExportRealm` (must succeed) → `ObjectStore.PutRealmExport` (must succeed — **the realm is never deleted without a confirmed durable export**, RP-EVT-4) → `DeleteRealmIfExists` → `RunInTx{Status:"deleted", ExportedAt, ExportS3Key}` + `outbox.Enqueue(TenantOffboarded{export_s3_key, offboarded_at})`. Terminal — RP-INV-5.
- An `operator`-sourced suspension is explicitly **excluded** from this auto-offboard path (§8.8) — only `billing_lapse` suspensions age toward offboarding.

Both branches use their own idempotency keys (`"lapse-suspend:<tenant_id>"`, `"offboard:<tenant_id>"`) via the same `provisioning_operations` ledger, independent of the HTTP-endpoint idempotency keys.

## RP-C4: Realm Config Reconcile

See RP-8 above — `*/10 * * * *`, retries every `realm_config_reconciliations` row in `status='pending'`.

## Idempotency Ledger Mechanics (§9.2)

`provisioning_operations`, PK = the caller-supplied `Idempotency-Key` (HTTP endpoints) or a service-constructed key (reconciler jobs, e.g. `"trial-expiry:<tenant_id>:<trial_ends_at>"`). `Begin(ctx, key, tenantID, operation)` inserts an `in_progress` row and returns `(nil, false, nil)` on first sight; a second caller with the same key gets `(existingRow, true, nil)` — `existingRow.Status == "succeeded"` replays `existingRow.Result` (JSON-decoded into the caller's expected type via the generic `replayOrConflict[T]` helper); any other status (`in_progress` — a concurrent call is still running, or `failed` — the prior attempt didn't complete) returns `409 operation_in_progress`, telling the caller to retry with the *same* key rather than starting a fresh operation. `Complete(ctx, key, resultJSON)` and `Fail(ctx, key)` are the two terminal transitions; both are called from within the service methods, `Complete` only after every Keycloak call and the local `RunInTx` have succeeded, `Fail`/`failIdempotency` from every early-return error path.

## Consumer Dedup Mechanics (`processed_events`)

Each of the three SQS consumers checks `ProcessedEventsStore.IsProcessed(consumer, eventID)` before doing any work and calls `MarkProcessed(consumer, eventID)` after — `consumer` is one of `membership`/`tenant`/`billing`, matching the queue. A redelivered SQS message (at-least-once delivery) is a guaranteed no-op on the second and subsequent deliveries.
