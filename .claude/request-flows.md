# Request Flows, Concurrency & GDPR

## Cross-Cutting Discipline: Keycloak-Effect-First, Then Local Commit (§9.1)

Every provisioning/lifecycle mutation in this service follows the same shape, visible in every method in `provisioning_service.go` and `lifecycle_service.go`:

1. `idempotency.Begin(ctx, key, tenantID, operation)` — if an existing row is found, either replay its `succeeded` result or return `409 operation_in_progress`.
2. Call Keycloak **first**, entirely **outside** any Postgres transaction.
3. Only on Keycloak success, open `tx.RunInTx(...)` and, in one transaction: update/insert the local row (`tenant_realms`, `keycloak_idp_configs`, etc.) **and** `Enqueue` the produced event(s) via the transactional outbox.
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
  slug := tenant_id.String()   -- the FULL uuid, not a truncated prefix (§9.3 fix below): Convert's request
                                  body carries no slug (§5.2), and this service persists no tenant_slug of
                                  its own, so a stable name is derived from the tenant id itself
  realmName := "tenant-" + slug
  shard := selectShard()
  realmMayHaveMigratedUsers := false
  Keycloak.CreateRealmFromTemplate(realmName, "v1")
    conflict (ErrRealmAlreadyDedicated) -> realmName is deterministic per tenant and current.Strategy is
                                            still "shared", so this can only be my OWN earlier partial
                                            attempt's realm -- resume against it, realmMayHaveMigratedUsers = true
    any other failure -> failIdempotency, return err (nothing created yet, no teardown needed)
  Keycloak.RegisterOIDCClients(realmName, slug)
    fail -> if !realmMayHaveMigratedUsers: bestEffortDeleteRealm(realmName); 502 conversion_failed
  sourceUsers := Keycloak.ListUsersByTenant(current.RealmName, tenant_id)
    fail -> if !realmMayHaveMigratedUsers: bestEffortDeleteRealm(realmName); 502 conversion_failed
  for each uid in sourceUsers:
    reenroll := Keycloak.MigrateUser(current.RealmName, realmName, uid)
      fail -> if !realmMayHaveMigratedUsers: bestEffortDeleteRealm(realmName)   -- see §9.3: once a user has
                                                                                    actually been migrated,
                                                                                    the realm must never be
                                                                                    torn down again
              502 conversion_failed
    realmMayHaveMigratedUsers = true
    reenroll -> webauthnReenroll++
  Keycloak.ForceLogoutRealm(current.RealmName)   -- best-effort, logged WARN on failure, non-fatal (sessions expire naturally)
  RunInTx:
    current.{Strategy:"dedicated", RealmName:realmName, KeycloakShard:shard, Status:"active", ConvertedAt:now()}
    tenant_realms.Update(current)
    outbox.Enqueue(TenantRealmReady{...})
    outbox.Enqueue(TenantConverted{..., users_migrated:len(sourceUsers), direct_signup:false})
  -> 200 {tenant_id, realm, strategy:"dedicated", keycloak_shard, oidc_clients, users_migrated, webauthn_reenroll_required, shared_sessions_terminated:true}
```

`MigrateUser` (in `internal/adapter/outbound/keycloak/`, not itself part of this service-layer flow but the mechanism the loop above calls per user) recreates the user in the target realm preserving the Keycloak `sub` (via the `partialImport` admin endpoint — the standard create-user endpoint ignores a caller-specified ID), copies the TOTP credential, and flags `WEBAUTHN_REGISTER` when the source held a WebAuthn credential (WebAuthn credentials cannot migrate across realms — the browser-bound authenticator is tied to the origin/realm it enrolled against). Because Keycloak's user ID is a **global** primary key across every realm in the deployment, the source user is deleted from the source realm *before* being recreated with the same ID in the target realm — a same-ID user cannot exist in two realms simultaneously. That delete is `isNotFound`-tolerant (matching every sibling delete in the same file): `retryTransport` can transparently retry a lost-response DELETE that actually succeeded, and without that tolerance the retry's 404 would abort the migration after the source user was already gone and never created in the target.

**§9.3 correctness fix — compensating teardown must never destroy an already-migrated user.** `bestEffortDeleteRealm` only ever runs while `realmMayHaveMigratedUsers` is still false — i.e. before `MigrateUser` has succeeded even once in this attempt, *and* before a resumed attempt's `CreateRealmFromTemplate` conflict has proven an earlier attempt already got that far. `MigrateUser` deletes each user from the source realm before creating them in the target, so once one user has been migrated, the target realm holds a user that exists nowhere else — deleting the realm at that point would be permanent, unrecoverable data loss, not a rollback. A retry with the same `Idempotency-Key` naturally resumes: the deterministic `realmName` collision is tolerated as proof of the earlier attempt, and a fresh `ListUsersByTenant` picks up only whatever remains in the shared realm.

## RP-4: Revert Conversion

```
POST /tenants/:id/revert-conversion
  current.Strategy != "dedicated" -> 400 invalid_request
  now - current.ConvertedAt > revertWindowDays*24h -> 409 revert_window_expired {converted_at}
  dedicatedRealm := current.RealmName
  sourceUsers := Keycloak.ListUsersByTenant(dedicatedRealm, tenant_id)
    ErrTenantRealmNotFound -> the dedicated realm is already gone even though the local row still says
                               "dedicated" -- can only mean an earlier attempt with this same
                               Idempotency-Key already migrated every user and deleted the realm (§9.1
                               Keycloak-effect-first) but failed before the local commit below landed;
                               resume straight to the local commit, skip migration/delete entirely
    any other failure -> 502 conversion_failed
  else:
    for each uid: Keycloak.MigrateUser(dedicatedRealm, sharedRealm, uid)     -- fail -> 502 conversion_failed
    Keycloak.DeleteRealmIfExists(dedicatedRealm)
  RunInTx:
    current.{Strategy:"shared", RealmName:sharedRealm, KeycloakShard:nil, ConvertedAt:nil, TrialEndsAt:now()+newTrialDurationDays, Status:"active"}
    tenant_realms.Update(current)
    outbox.Enqueue(TrialReactivated{tenant_id, new_trial_ends_at})
  -> 200 {tenant_id, realm:"trial", strategy:"shared"}
```

**Correctness fix — a local-commit failure after a successful realm delete must not permanently strand the tenant.** Before this fix, `DeleteRealmIfExists` ran unconditionally and unwrapped; if it succeeded but the subsequent `RunInTx` failed (an ordinary DB hiccup), the local row was left claiming `Strategy=dedicated` against a realm that no longer existed in Keycloak, and every retry's `ListUsersByTenant` hard-failed against the absent realm forever — the tenant could neither revert nor convert without an operator manually fixing the row. `Admin.ListUsersByTenant` now returns a distinguishable `ErrTenantRealmNotFound` on a 404 (reliable here — a realm that exists but matches zero users returns `200` with an empty list, never `404`), which `RevertConversion` treats as the resume signal above.

## RP-6/RP-7: IdP Configuration Under the §11.6 Gate

```
PUT /tenants/:id/idp {idp_alias, protocol, display_name, config{non-secret}, secret, acknowledgements[]}
  !idp_alias matches ^[a-zA-Z0-9_-]{1,64}$ -> 400 invalid_request   -- checked BEFORE idempotency.Begin; see below
  idempotency.Begin(key, tenant_id, "configure_idp")
    found + succeeded -> replay
    found + other     -> 409 operation_in_progress
  realm := tenant_realms.FindByTenantID(tenant_id)
  realm.Strategy != "dedicated" -> failIdempotency, 409 idp_requires_dedicated_realm
  validation := ValidateIdpConfig(protocol, config, acknowledgements)  -- §11.6 checklist, see below
  keycloak_idp_config_validations.Insert(tenant_id, idp_alias, ValidatorVersion, outcome, per-check results, acks)
    (best-effort — a failure to record is logged WARN, not propagated)
  outcome == failing        -> failIdempotency, 400 idp_region_portability_failed {failed_checks, warnings}
  outcome == warnings_only  -> failIdempotency, 422 idp_warnings_unacknowledged {warnings}
  -- else (pass, or warnings_only fully acknowledged):
  isNewConfig := keycloak_idp_configs.FindByAlias(tenant_id, idp_alias) returns ErrIdpConfigNotFound
  if secret != "": OpenBao.WriteIdpSecret(tenant_id, idp_alias, {secret})   -- fail -> failIdempotency, return err
  Keycloak.CreateOrUpdateIdp(realm.RealmName, {alias, protocol, display_name, enabled:true, config, secret})
    fail -> if secret != "" AND isNewConfig: OpenBao.DeleteIdpSecret(tenant_id, idp_alias)   -- compensating cleanup,
                                              see below; failIdempotency, return err
  keycloak_idp_configs.Upsert{tenant_id, idp_alias, protocol, display_name, enabled:true, config, status:"active"}
    fail -> failIdempotency, return err
  idempotency.Complete(key, result)
  -> 200 {idp_alias, status:"active"}

DELETE /tenants/:id/idp/:alias
  !alias matches ^[a-zA-Z0-9_-]{1,64}$ -> 400 invalid_request
  realm := tenant_realms.FindByTenantID(tenant_id)
  Keycloak.DeleteIdp(realm.RealmName, alias)
  OpenBao.DeleteIdpSecret(tenant_id, alias)
  keycloak_idp_configs.Delete(tenant_id, alias)
  -> 204
```

**`idp_alias` format validation (security fix).** `openbao.Client.idpPath` builds the OpenBao KV path (`secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>`) via plain string interpolation, with no sanitization at that layer — `idp_alias` otherwise flowed there unvalidated all the way from RP-6's JSON body and RP-7's URL path param, with no format constraint anywhere in the stack. A caller-supplied alias containing a path-traversal sequence (e.g. `"../<other-tenant-id>/evil"`) could have resolved outside the intended tenant-scoped path. `IdpService.ConfigureIdp`/`RemoveIdp` — the single choke point both routes share — now reject anything outside `^[a-zA-Z0-9_-]{1,64}$` before any OpenBao/Keycloak call; no dot is permitted at all, so `.`/`..` can never appear as a path component.

**RP-6 is now ledgered against `Idempotency-Key` like every other mutating endpoint (§9.2).** The route has always enforced `RequireIdempotencyKey`, but `ConfigureIdp` never actually called the idempotency store — a retry re-ran the entire flow (re-validating, re-recording an audit row, re-writing OpenBao, re-calling Keycloak) instead of replaying the stored result. It also adds a compensating OpenBao-secret delete when `CreateOrUpdateIdp` fails **after** the secret was already written — but only when `isNewConfig` (confirmed via `FindByAlias`): an update to an already-configured alias never deletes on failure, since the pre-existing Keycloak IdP is still live and completely unaffected by the failed update, even though `WriteIdpSecret` already overwrote OpenBao's copy with the new, never-applied value.

**§11.6 Region-Agnostic IdP Configuration Checklist** (`idp_validator.go`): checks every SAML/OIDC config field against a fixed list of AWS region substrings (`ap-south-1`, `us-east-1`, etc.) and against the one platform-wide region-neutral auth host (`auth.bcbpsolutions.com`). CRITICAL checks (1-6, 10-12) — SSO URL region (1), SLO URL region (2), customer signing cert present (3), ACS URL points at the neutral host (4), SLO **response** URL region (5, examines a distinct `slo_response_url` config key — SAML's `SingleLogoutService` `ResponseLocation`, defaulting to `slo_url` when a caller never sets it), entity ID region (6), OIDC issuer/redirect/audience region (10-12) — **block** with `400`. WARN checks (7-9, 13) — SAML `NameIDFormat != "transient"`, attribute-mapper URNs region-free, strong signature algorithm (`RSA-SHA(256|384|512)`), OIDC PKCE `S256` — require a recorded acknowledgement (by check ID) or return `422`. RP-13's whole-estate audit re-runs the same `ValidateIdpConfig` function against every stored config's non-secret `Config` (never re-reading secrets from OpenBao) to compute pass/warnings/failing counts.

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
RP-C4 (`*/10 * * * *`) sweeps every `status='pending'` row: re-resolves the realm, re-attempts `ApplyRealmConfig`; success → `MarkApplied`; failure → `MarkFailed(lastErr, RECONCILER_MAX_ATTEMPTS)`, which leaves the row pending for the next tick unless the attempt count it just recorded has reached `RECONCILER_MAX_ATTEMPTS` (default 20), in which case the row moves atomically to the terminal `failed` status instead — a poison-pill cap so a permanently-broken setting stops consuming a `ListPending` batch slot on every tick forever and surfaces for operator attention. This is the one place in the service where a client-facing 200 response does **not** mean the Keycloak-side effect has actually landed yet — by design, since blocking the caller on Keycloak availability for a config patch would be worse than converging asynchronously.

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

`0 2 * * *`. Two phases sharing one `jctx.BatchLimit` budget (`remaining := jctx.BatchLimit - suspendPhase.Attempted`) so a busy suspend phase can't starve the offboard phase of its own daily run, or vice versa:

- **Suspend phase** — polls O&M's I-16 `GET /api/v1/internal/subscription-lapses` via `jctx.SubscriptionLapses.ListLapsedSubscriptions` (`port.SubscriptionLapseReader`, `internal/adapter/outbound/orgmembership`). O&M itself applies `CANCELLED_GRACE_DAYS` before a tenant appears in this list — RP does not re-derive the grace window. Only `realm_type=="dedicated"` entries are acted on (`shared` entries are counted `Skipped`, per §8.8 — trial/shared realms go through RP-C1/RP-C2 instead). Per dedicated tenant, `suspendLapsedTenant`: skip as a no-op if the realm is already `suspended`/`deleted` (never touches the idempotency ledger); otherwise `DisableRealm` → `RunInTx{Status:"suspended", DisabledAt:now(), SuspensionSource:"billing_lapse"}` + `outbox.Enqueue(TenantSuspended{source:"billing_lapse"})`, keyed `"lapse-suspend:<tenant_id>"`. If `jctx.SubscriptionLapses` is `nil` (`ORG_MEMBERSHIP_BASE_URL` unset), this phase logs a WARN and skips entirely — the offboard phase below still runs.
- **Offboard phase** — always runs, independent of whether the suspend phase ran. Queries `TenantRealmRepository.ListSuspendedPastRetention(retentionDays=RETENTION_DAYS, limit=remaining)`, which is scoped to `status='suspended' AND suspension_source='billing_lapse' AND disabled_at < now() - RETENTION_DAYS` (partial index `idx_tenant_realms_lapse_offboard`) — **an `operator`-sourced suspension (RP-14) is structurally excluded from this query**, not just skipped in code (§8.8): only `billing_lapse` suspensions age toward offboarding. Per candidate, `offboardTenant`: `ExportRealm` (must succeed) → `ObjectStore.PutRealmExport` (must succeed — **the realm is never deleted without a confirmed durable export**, RP-EVT-4) → `DeleteRealmIfExists` → `RunInTx{Status:"deleted", ExportedAt, ExportS3Key}` + `outbox.Enqueue(TenantOffboarded{export_s3_key, offboarded_at})`, keyed `"offboard:<tenant_id>"`. Terminal — RP-INV-5.

Both phases use their own idempotency keys via the same `provisioning_operations` ledger, independent of the HTTP-endpoint idempotency keys. `suspension_source` is the durable column on `tenant_realms` that makes the offboard phase's query possible — it is also set by RP-14's `Suspend` (`operator`) and cleared by RP-10's `Reactivate`, so a tenant reactivated after either kind of suspension never re-enters this sweep.

## RP-C4: Realm Config Reconcile

See RP-8 above — `*/10 * * * *`, retries every `realm_config_reconciliations` row in `status='pending'`.

## Idempotency Ledger Mechanics (§9.2)

`provisioning_operations`, PK = the caller-supplied `Idempotency-Key` (HTTP endpoints) or a service-constructed key (reconciler jobs, e.g. `"trial-expiry:<tenant_id>:<trial_ends_at>"`). `Begin(ctx, key, tenantID, operation)` inserts an `in_progress` row and returns `(nil, false, nil)` on first sight; a second caller with the same key gets `(existingRow, true, nil)` — `existingRow.Status == "succeeded"` replays `existingRow.Result` (JSON-decoded into the caller's expected type via the generic `replayOrConflict[T]` helper); any other status (`in_progress` — a concurrent call is still running, or `failed` — the prior attempt didn't complete) returns `409 operation_in_progress`, telling the caller to retry with the *same* key rather than starting a fresh operation. `Complete(ctx, key, resultJSON)` and `Fail(ctx, key)` are the two terminal transitions; both are called from within the service methods, `Complete` only after every Keycloak call and the local `RunInTx` have succeeded, `Fail`/`failIdempotency` from every early-return error path.

## Consumer Dedup Mechanics (`processed_events`)

Each of the three SQS consumers is a `platform-events` `Handler`. Unknown types are silently ack'd, counted, and recorded in `processed_events` inside `RunInTx` (forward-compat). Known types check `ProcessedEventsStore.IsProcessed(consumer, eventID)` first (a hit increments `processed_events_duplicates_total`) and call `MarkProcessed` inside `TxRunner.RunInTx` after the effect — `consumer` is one of `membership`/`tenant`/`billing`. A redelivered SQS message (at-least-once delivery) is a guaranteed no-op on the second and subsequent deliveries.

## GDPR & Data Lifecycle

### Scenario → Flow Reference

| Scenario | Flow | Trigger |
|---|---|---|
| Trial expiry & hard delete | RP-C1 / RP-C2 above | `trial_ends_at` passed → disable; `TRIAL_GRACE_DAYS` past that → hard-delete |
| Tenant offboarding (paid) | RP-C3 above | Billing-lapse suspend → `RETENTION_DAYS` past that → export + hard-delete |
| Idempotent user delete (invitee cleanup, O&M-initiated) | RP-15/RP-16 above | O&M `DeleteUser` — Keycloak-effect only, no local PII to scrub here |

This service holds **no per-user table** (RP-5/RP-9/RP-15/RP-16 persist nothing locally) — user-level PII erasure is entirely a Keycloak-side effect (`HardDeleteUsersInRealm`/`DeleteUser`), not a local-row scrub. The only local rows this section is about are `tenant_realms` and the realm-level artifacts (IdP config, exports).

### Trial Expiry & Hard Delete PII Scrub (RP-C1 / RP-C2)

RP-C1 disables the shared-realm trial users at `trial_ends_at` (no PII removed yet — reactivatable). RP-C2, at `TRIAL_GRACE_DAYS` past that, hard-deletes the trial users from Keycloak via the Admin API (removes email/name/profile) and transitions the local `tenant_realms` row to `deleted` — no residual PII is retained locally (`tenant_realms` carries no personal fields), only `tenant_id` for the audit FK. The immutable Keycloak `sub` is preserved for audit continuity even though the user itself is gone. No event is emitted on hard delete — O&M runs its own independent cleanup cron for its `tenants` row; the two services do not coordinate this specific step (see RP-C2 above).

### Tenant Offboarding PII Handling (RP-C3)

Export-before-delete is the load-bearing invariant (RP-EVT-4): `ExportRealm` → `ObjectStore.PutRealmExport` (KMS-encrypted, for compliance retention) must both succeed **before** `DeleteRealmIfExists` ever runs — the realm, and every credential/session/profile it holds, is never destroyed without a durable, encrypted export existing first. `TenantOffboarded{export_s3_key}` carries the pointer so downstream consumers (O&M's own GDPR wipe) know the export landed. `offboarded` is terminal (RP-INV-5) — no path back in.

### Residency

Single-region (`ap-south-1`) — RDS, S3, and the Keycloak cluster all live there. The §11.6 region-portability gate exists specifically so a customer-supplied IdP configuration can never pin this platform to a different region ahead of a future cross-region migration.

### Data Retention

| Table / artifact | Retention | Mechanism |
|---|---|---|
| `tenant_realms` (live) | Life of the tenant | — |
| `tenant_realms` (soft-deleted) | Retained for the audit FK, pruned per the platform schedule | — |
| `keycloak_idp_config_validations` | Append-only, never pruned | — |
| `processed_events` | 8 days (`PROCESSED_EVENTS_TTL_DAYS`, deliberately > the 7-day SQS message lifetime) | `processed-events-prune` CronJob |
| `outbox_events` (published) | Operational retention window | `outbox-prune` CronJob |
| S3 realm exports | Compliance retention schedule (SSE-KMS) | Bucket lifecycle policy, not application code |

### PII Boundary

The `provisioner` database holds **minimal PII by design**: `keycloak_idp_configs.config` may carry organizational (not personal) URLs/entity IDs; `provisioning_operations.result` transiently mirrors a provisioning response and can include `admin_email` for the life of that idempotency-ledger row. Real PII — names, emails, credentials, TOTP/WebAuthn secrets — lives exclusively in Keycloak, never mirrored here; `tenant_realms` itself carries no personal fields at all. This is the flip side of having no per-user table: there is very little for this service's own GDPR erasure path to do beyond the Keycloak-effect calls RP-C1/RP-C2/RP-C3 already make.
