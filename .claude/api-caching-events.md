# API & Events

## API Conventions (§5.1)

All 16 endpoints live under **one** prefix: `/api/v1/internal/*` — this service has almost no public surface; every route is mesh-internal, called by the Signup BFF, Org & Membership, Billing (via events, not directly), or operator tooling. Three infra routes (`/healthz`, `/readyz`) and two docs routes (`/asyncapi`, `/asyncapi.yaml`) sit outside that group, unauthenticated. `/metrics` is served on a **separate port** (`METRICS_PORT=9090`), never on the main router.

Middleware chain (`router.go`): 1 MB body-size cap → `gincommon.TimeoutMiddleware(30s)` → `gincommon.ObservabilityMiddlewares` → (per-route-group) `gincommon.ProtectedMiddlewares` → `GUCBridgeMiddleware` (binds `app.tenant_id` transaction-locally) → `RequireJSONContentType` → `RequireSystemRole` (defense-in-depth; mesh mTLS + NetworkPolicy is primary) → per-route `RequireIdempotencyKey` / `RequireOperatorRole` where applicable.

## Endpoint Catalogue (RP-1..RP-16)

| # | Method & Path | AuthZ | Idempotency-Key | Purpose |
|---|---|---|---|---|
| RP-1 | `POST /tenants/provision-trial` | system | required | Create a trial user in the shared realm, stamp `tenant_id`, trigger `VERIFY_EMAIL`. Emits `TrialTenantProvisioned`. |
| RP-2 | `POST /tenants/provision-direct` | system | required | Provision a **dedicated** realm straight away (no shared-realm intermediate): create realm from template, register OIDC clients, create admin user. Emits `TenantRealmReady` + `DirectPaidSignup` + `TenantConverted{direct_signup:true}`. |
| RP-3 | `POST /tenants/:id/convert` | system | required | Trial → dedicated conversion: create dedicated realm, migrate every trial user (preserving `sub`, copying TOTP, flagging WebAuthn re-enrollment), force-logout the shared realm. Emits `TenantRealmReady` + `TenantConverted{direct_signup:false}`. |
| RP-4 | `POST /tenants/:id/revert-conversion` | system | required | Undo Convert within the reversal window: migrate users back to the shared realm, delete the dedicated realm, extend the trial. Emits `TrialReactivated`. `409 revert_window_expired` past the window. |
| RP-5 | `POST /tenants/:id/users` | system | required | Create an invited user in the tenant's current realm with `RequiredActions` applied **verbatim** (never re-derived). No local persistence — no per-user table exists. |
| RP-6 | `PUT /tenants/:id/idp` | system | required | Configure (create/update) a SAML/OIDC IdP — behind the §11.6 region-portability gate. Requires a `dedicated` realm (`409 idp_requires_dedicated_realm` otherwise). Writes the secret to OpenBao, then calls Keycloak, then upserts the local non-secret config row. |
| RP-7 | `DELETE /tenants/:id/idp/:alias` | system | — | Remove an IdP — deletes from Keycloak, deletes the OpenBao secret, deletes the local row. Idempotent. |
| RP-8 | `PATCH /tenants/:id/realm-config` | system | — | Apply an allow-listed realm-level setting (`local_accounts_enabled`, `idp:<alias>`) synchronously; on transient Keycloak failure, records a `pending` `realm_config_reconciliations` row for RP-C4 instead of failing the caller (`RP-INV-6`, local-intent-first). `400 unsupported_realm_setting` off the allow-list. |
| RP-9 | `POST /tenants/:id/users/:keycloak_user_id/mfa-reset` | system | — | Delete every OTP/WebAuthn credential; re-adds `CONFIGURE_TOTP` if the user still holds `requires-mfa`. Fail-closed — no reconciler, no local state. Route param is internally named `:keycloak_user_id` (see note below). |
| RP-10 | `POST /tenants/:id/reactivate` | system | required | Re-enable a disabled trial user or a suspended dedicated realm (billing-lapse **or** operator-sourced — one endpoint reverses both). `409 tenant_offboarded` if terminal. Idempotent no-op if already active. Emits `TrialReactivated` or `TenantReactivated`. |
| RP-11 | `GET /tenants/:id/realm` | system | — | Read-only: current realm name/strategy/shard/status. |
| RP-12 | `POST /tenants/:id/expire-trial` | system | required | Idempotent per-tenant trial-expiry effect (disable shared-realm users, mark `disabled_at`, emit `TrialExpired`) — called once per row by RP-C1; also directly callable. |
| RP-13 | `POST /audit/idp-region-portability` | **operator** | — | Whole-estate, **not tenant-scoped** — re-validates every stored IdP config's non-secret `Config` against the §11.6 checklist (does not re-read secrets from OpenBao) and returns pass/warnings/failing counts + the list of failing tenant IDs. |
| RP-14 | `POST /tenants/:id/suspend` | **operator** | required | Emergency operator-triggered lockout — disables the dedicated realm (or just that tenant's shared-realm users). Effect-only: never sets any subscription-status field (that's O&M's). Reversible via RP-10. Emits `TenantSuspended{source:"operator"}`. |
| RP-15 | `DELETE /tenants/:id/users/:keycloak_user_id` | system | — | Idempotent Keycloak user delete — deleting an absent user is a success (backs O&M's PI-9-style reconciler). |
| RP-16 | `POST /tenants/:id/users/:keycloak_user_id/revoke-sessions` | system | — | Idempotent session/token revocation, best-effort/fail-open from the caller's perspective. |

**RP-9's route param note:** the frozen §23 path spells the segment `:uid`; Gin requires one wildcard name per path position across sibling routes sharing a prefix, and RP-15/RP-16 already use `:keycloak_user_id` at that position — so RP-9 uses the same internal capture name. This is a routing-internals detail only; the actual URL a caller hits is unchanged, and no wire contract is broken.

Every mutating endpoint above marked "required" enforces `RequireIdempotencyKey` — a missing header is `400 missing_idempotency_key`; a reused key against an in-flight or previously-failed operation is `409 operation_in_progress`; a reused key against a `succeeded` operation replays the stored JSON result rather than re-executing.

## Error Taxonomy (§17, `internal/core/domain/errors.go`)

| Code | HTTP | Trigger |
|---|---|---|
| `invalid_request` | 400 | Additive (not in the frozen §17 list) — generic malformed-JSON/failed-binding-validation |
| `invalid_realm_strategy` | 400 | |
| `invalid_idp_protocol` | 400 | |
| `unsupported_realm_setting` | 400 | RP-8 setting_key off the allow-list |
| `idp_region_portability_failed` | 400 | RP-6 — one or more §11.6 CRITICAL checks failed |
| `missing_idempotency_key` | 400 | Mutating route missing `Idempotency-Key` header |
| `idp_warnings_unacknowledged` | 422 | RP-6 — one or more §11.6 WARN checks lack a recorded acknowledgement |
| `tenant_realm_not_found` | 404 | |
| `idp_config_not_found` | 404 | |
| `idp_requires_dedicated_realm` | 409 | RP-6 against a `shared`-strategy tenant |
| `realm_already_dedicated` | 409 | RP-3 Convert on an already-dedicated tenant |
| `revert_window_expired` | 409 | RP-4 past the reversal window |
| `tenant_offboarded` | 409 | Any mutation against a terminal (offboarded) tenant |
| `operation_in_progress` | 409 | Idempotency-Key replay against a non-`succeeded` row |
| `optimistic_lock_conflict` | 409 | |
| `keycloak_unavailable` | 502 | |
| `conversion_failed` | 502 | RP-3/RP-4 Keycloak sequence failure (with `WithDetails` context) |
| `org_membership_unavailable` | 503 | O&M I-8 residual-MFA check unreachable (membership-realm-q consumer) |
| `object_store_unavailable` | 503 | S3 realm-export failure (RP-C3 offboarding) |

`Error.Status()` derives the HTTP code from `Code` via a lookup table; an unrecognized code defaults to 500. `Error.WithDetails(map[string]any)` attaches structured context echoed into the response body's `details` field.

## Events

### Produced — `iam-tenant-events` SNS topic, 9 event types (§23, §7.5)

Envelope `source = "iam-realm-provisioner"`. Cron-originated events (RP-C1/RP-C3) use the sentinel actor `SystemActorID = "00000000-0000-0000-0000-0000000000a1"`.

| Event | Producer | Key payload fields |
|---|---|---|
| `TrialTenantProvisioned` | RP-1 | `tenant_id`, `realm`, `keycloak_user_id` |
| `TenantRealmReady` | RP-2, RP-3 | `tenant_id`, `realm`, `keycloak_shard`, `oidc_clients[]` |
| `TenantConverted` | RP-2 (`direct_signup:true`), RP-3 (`direct_signup:false`) | `tenant_id`, `realm`, `plan`, `billing_subscription_id?`, `users_migrated`, `direct_signup` |
| `DirectPaidSignup` | RP-2 | `tenant_id`, `realm`, `direct_signup` |
| `TrialExpired` | RP-C1 (sweep) / RP-12 (direct call) | `tenant_id`, `trial_ends_at`, `grace_ends_at` |
| `TrialReactivated` | RP-4 (revert), RP-10 (shared-strategy reactivate) | `tenant_id`, `new_trial_ends_at` |
| `TenantSuspended` | RP-C3 (`source:"billing_lapse"`), RP-14 (`source:"operator"`) | `tenant_id`, `realm`, `cancelled_at?`, `suspended_at`, `source`, `reason?` |
| `TenantOffboarded` | RP-C3 (post-export-confirmed delete) | `tenant_id`, `realm`, `export_s3_key`, `offboarded_at` |
| `TenantReactivated` | RP-10 (dedicated-strategy reactivate) | `tenant_id`, `realm`, `source`, `reason?` |

Every payload is validated against an embedded JSON Schema (`internal/eventschema/*.json`) at outbox-enqueue time (shape only — wire encoding via the Glue codec happens later, at SNS-publish time). `internal/eventschema/asyncapi_drift_test.go` asserts `api/asyncapi.yaml`'s `iamTenantEvents` channel names exactly these 9 events and no others.

### Consumed — 3 SQS queues, 5 distinct event types (§7.1)

| Queue | `EventType` filter | Source topic | Drives |
|---|---|---|---|
| `membership-realm-q` | `DepartmentMembershipGranted`, `DepartmentMembershipLevelChanged`, `TenantRoleGranted`, `TenantRoleRevoked` | O&M's `iam.membership.events` | `MfaService.SyncRequiresMfa` — grant path unconditional assign-if-absent; revoke path (`TenantRoleRevoked` only) consults O&M's I-8 residual-MFA-requirement check before revoking |
| `tenant-realm-q` | `TrialStarted` | O&M's `iam.tenant.events` | Idempotent upsert of `trial_ends_at` onto the tenant's `tenant_realms` row — this service never queries O&M for trial timing, it's push-only |
| `billing-realm-q` | `TenantReactivated` | Billing's `billing.events` (same event *name* as this service's own produced `TenantReactivated`, but a different topic/producer — no self-consumption) | `LifecycleService`-equivalent reactivation — re-enables a `suspended` dedicated realm only; no-op otherwise (still recorded in `processed_events`) |

Each queue has a `-dlq` sibling, `maxReceiveCount=5`. Dedup via `processed_events` PK `(event_id, consumer)`, `consumer ∈ {membership, tenant, billing}`.

### AsyncAPI & Schema Drift

`api/asyncapi.yaml` — design-time source of truth, embedded via `api/embed.go`, served live at `/asyncapi` and `/asyncapi.yaml` (gated by `DOCS_ENABLED`/`DOCS_AUTH_TOKEN` in production). Declares 4 servers (the SNS topic + 3 SQS queues), 4 channels, and per-message schemas. This repo has no live AWS Glue Schema Registry comparison wired into CI (unlike some sibling services' `platform-schemagov` pipeline) — `internal/eventschema/asyncapi_drift_test.go` is the CI-enforceable substitute: it structurally parses the `iamTenantEvents` channel's `messages:` block out of the YAML and asserts its key set equals `eventschema.ByEventType`'s map keys exactly.
