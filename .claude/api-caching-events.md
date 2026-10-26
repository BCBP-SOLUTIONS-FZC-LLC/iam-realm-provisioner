# API & Events

## API Conventions (§5.1)

All 17 endpoints live under **one** prefix: `/api/v1/internal/*` — this service has almost no public surface; every route is mesh-internal, called by the Signup BFF, Org & Membership, Billing (via events, not directly), or operator tooling. Three infra routes (`/healthz`, `/readyz`) and two docs routes (`/asyncapi`, `/asyncapi.yaml`) sit outside that group, unauthenticated. `/metrics` is served on a **separate port** (`METRICS_PORT=9090`), never on the main router.

Middleware chain (`router.go`): 1 MB body-size cap → `gincommon.ObservabilityMiddlewares` (with `TimeoutMiddleware(30s)` innermost, via `Config.RequestTimeout`) → (per-route-group) `gincommon.ProtectedMiddlewares` → `GUCBridgeMiddleware` (binds `app.tenant_id` transaction-locally) → `RequireJSONContentType` → `RequireSystemRole` (defense-in-depth; mesh mTLS + NetworkPolicy is primary) → per-route `RequireIdempotencyKey` / `RequireOperatorRole` where applicable.

## Endpoint Catalogue (RP-1..RP-17)

| # | Method & Path | AuthZ | Idempotency-Key | Purpose |
|---|---|---|---|---|
| RP-1 | `POST /tenants/provision-trial` | system | required | Create a trial user in the shared realm, stamp `tenant_id`, trigger `VERIFY_EMAIL`. `plan` ∈ `starter`\|`pro`\|`enterprise`. Requires `terms_acceptance {version, accepted_at, ip_address, user_agent}` (LLD v1.41 — `version` `[A-Za-z0-9._-]{1,32}`, `accepted_at` within [now−24h, now+10m], IPv4/IPv6, UA ≤ 512 bytes no control chars; else 400 `INVALID_REQUEST`), recorded once per owner+version as Audit Log `legal.terms.accepted` via `pending_audit_entries` — never on the bus. Emits `TenantIdentityProvisioned`. |
| RP-2 | `POST /tenants/provision-direct` | system | required | Provision a **dedicated** realm straight away (no shared-realm intermediate): create realm from template, register OIDC clients, create admin user. Requires `terms_acceptance` exactly as RP-1. Emits `TenantIdentityProvisioned` + `TenantRealmReady` + `DirectPaidSignup` + `TenantConverted{direct_signup:true}`. |
| RP-3 | `POST /tenants/:id/convert` | system | required | Trial → dedicated conversion: create dedicated realm, migrate every trial user (preserving `sub`, copying TOTP, flagging WebAuthn re-enrollment), force-logout the shared realm. Emits `TenantRealmReady` + `TenantConverted{direct_signup:false}`. Post-commit, revokes the old shared-realm credential via TS-2 (`RevokeCredential`); a failed revoke is logged and does not fail the conversion. |
| RP-4 | `POST /tenants/:id/revert-conversion` | system | required | Undo Convert within the 7-day reversal window: migrate users back to the shared realm, delete the dedicated realm, re-mint the tenant's `platform-automation` client back in the shared realm (§2.5 — the dedicated realm's client died with the realm), extend the trial. Emits `TrialReactivated`. `409 revert_window_expired` past the window. Named callers: Billing (on fraud/chargeback dispute; which Stripe signal triggers it is a Billing-side decision, CD-10). |
| RP-5 | `POST /tenants/:id/users` | system | required | Create an invited user in the tenant's current realm with `RequiredActions` applied **verbatim** (never re-derived). No local persistence — no per-user table exists. |
| RP-6 | `PUT /tenants/:id/idp` | system | required | Configure (create/update) a SAML/OIDC IdP — behind the §11.6 region-portability gate. Requires a `dedicated` realm (`409 idp_requires_dedicated_realm` otherwise). Writes the secret to OpenBao, then calls Keycloak, then upserts the local non-secret config row. |
| RP-7 | `DELETE /tenants/:id/idp/:alias` | system | — | Remove an IdP — deletes from Keycloak, deletes the OpenBao secret, deletes the local row and enqueues its `TenantIdpConfigChanged` audit entry in the same tx. Idempotent. |
| RP-8 | `PATCH /tenants/:id/realm-config` | system | — | Apply an allow-listed realm-level setting (`local_accounts_enabled`, `idp:<alias>`) synchronously; on transient Keycloak failure, records a `pending` `realm_config_reconciliations` row for RP-C4 instead of failing the caller (`RP-INV-6`, local-intent-first). `400 unsupported_realm_setting` off the allow-list. |
| RP-9 | `POST /tenants/:id/users/:keycloak_user_id/mfa-reset` | system | — | Delete every OTP/WebAuthn credential; re-adds `CONFIGURE_TOTP` if the user still holds `requires-mfa`. Fail-closed — no reconciler, no local state. Route param is internally named `:keycloak_user_id` (see note below). |
| RP-10 | `POST /tenants/:id/reactivate` | system | required | Re-enable a disabled trial user or a suspended dedicated realm (billing-lapse **or** operator-sourced — one endpoint reverses both). `409 tenant_offboarded` if terminal. Idempotent no-op if already active. Emits `TrialReactivated` or `TenantReactivated`. |
| RP-11 | `GET /tenants/:id/realm` | system | — | Read-only: current realm name/strategy/shard/status, and `idp_count` (count of active SAML/OIDC IdP configurations — CD-20 interim SSO-downgrade guard for Billing while Core builds `plan-change-check`). Named callers: Signup BFF, Org & Membership, Billing. |
| RP-12 | `POST /tenants/:id/expire-trial` | system | required | Idempotent per-tenant trial-expiry effect (disable shared-realm users, mark `disabled_at`, emit `TrialExpired`) — called once per row by RP-C1; also directly callable. |
| RP-13 | `POST /audit/idp-region-portability` | **operator** | — | Whole-estate, **not tenant-scoped** — re-validates every stored IdP config's non-secret `Config` against the §11.6 checklist (does not re-read secrets from OpenBao) and returns pass/warnings/failing counts + the list of failing tenant IDs. |
| RP-14 | `POST /tenants/:id/suspend` | **operator** | required | Emergency operator-triggered lockout — disables the dedicated realm (or just that tenant's shared-realm users). Effect-only: never sets any subscription-status field (that's O&M's). Reversible via RP-10. Emits `TenantSuspended{source:"operator"}`. |
| RP-15 | `DELETE /tenants/:id/users/:keycloak_user_id` | system | — | Idempotent Keycloak user delete — deleting an absent user is a success (backs O&M's PI-9-style reconciler). |
| RP-16 | `POST /tenants/:id/users/:keycloak_user_id/revoke-sessions` | system | — | Idempotent session/token revocation, best-effort/fail-open from the caller's perspective. |
| RP-17 | `POST /tenants/:id/service-account/keys/refresh` | system | — | Refreshes Keycloak's cached JWKS keys for the tenant's realm (`ClearServiceAccountKeysCache`, EXT-6) — required after a rotate (Token Service's TS-1) for the new key to be recognized, and after a revoke (TS-2) for the old key to actually stop authenticating; no Keycloak-side step happens at mint time (§2.5). Empty body, no idempotency key (clearing an already-fresh cache is a no-op); fail-closed on Keycloak-unreachable. `422 service_account_not_provisioned` if never minted. |

**RP-9's route param note:** the frozen §25 path spells the segment `:uid`; Gin requires one wildcard name per path position across sibling routes sharing a prefix, and RP-15/RP-16 already use `:keycloak_user_id` at that position — so RP-9 uses the same internal capture name. This is a routing-internals detail only; the actual URL a caller hits is unchanged, and no wire contract is broken.

Every mutating endpoint above marked "required" enforces `RequireIdempotencyKey` — a missing header is `400 missing_idempotency_key`; a reused key against an in-flight operation is `409 operation_in_progress`; a reused key against a `failed` operation, or an `in_progress` one older than `IDEMPOTENCY_IN_PROGRESS_LEASE` (15m — its pod died), is reclaimed and re-executed (every step is a Keycloak-effect-first reconcile, so it converges); a reused key against a `succeeded` operation replays the stored JSON result rather than re-executing.

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
| `keycloak_user_not_found` | 404 | Additive — internal-only signal from `FindUserByEmail`, absorbed before reaching a caller (§9.2) |
| `idp_requires_dedicated_realm` | 409 | RP-6 against a `shared`-strategy tenant |
| `realm_already_dedicated` | 409 | RP-3 Convert on an already-dedicated tenant; RP-12 ExpireTrial on a non-shared-strategy tenant (already converted, nothing to expire) |
| `revert_window_expired` | 409 | RP-4 past the reversal window |
| `tenant_offboarded` | 409 | Any mutation against a terminal (offboarded) tenant |
| `operation_in_progress` | 409 | Idempotency-Key replay against a non-`succeeded` row |
| `optimistic_lock_conflict` | 409 | `record_version` mismatch on a `tenant_realms`/`idp_config` update; also surfaced from `IssueOrRotateCredential` when Token Service's TS-1 409 body names this cause explicitly (disambiguated from `service_account_rotation_in_flight` and `credential_replay_revoked` below by decoding the RFC 9457 problem's `errorCode` (`OPTIMISTIC_LOCK_CONFLICT`), not flattened into one code) |
| `user_email_conflict` | 409 | Additive — RP-1/RP-2/RP-5 create-user 409 that `resolveUserCreateConflict` could not resolve as the caller's own lost-response retry (§9.2); wire RFC 9457 `errorCode`: `EMAIL_ALREADY_REGISTERED` (rev 1.42; was `USER_EMAIL_CONFLICT`) |
| `service_account_rotation_in_flight` | 409 | Additive — Token Service TS-1 rejected a concurrent issue/rotate for the same principal (§2.5); also the safe default for any other/unparseable TS-1 409 body |
| `credential_replay_revoked` | 409 | Additive — Token Service TS-1 (TS-D13) rejected a replayed `rotation_id` whose credential has since been revoked; distinct from `service_account_rotation_in_flight` so callers can tell "a live rotation race" from "your stored rotation_id is stale" |
| `credential_replay_expired` | 409 | Additive — Token Service TS-1 (TS-D22) rejected a replayed `rotation_id` whose credential has since expired (distinct from `credential_replay_revoked` where the credential was explicitly revoked); caller should generate a fresh `rotation_id` |
| `service_account_not_provisioned` | 422 | Additive — RP-17 apply attempted before the tenant's `platform-automation` principal was ever minted |
| `keycloak_unavailable` | 502 | |
| `conversion_failed` | 502 | RP-3/RP-4 Keycloak sequence failure (with `WithDetails` context) |
| `org_membership_unavailable` | 503 | O&M I-8 residual-MFA check unreachable (membership-realm-q consumer) |
| `object_store_unavailable` | 503 | S3 realm-export failure (RP-C3 offboarding) |
| `db_unavailable` | 503 | Postgres connectivity / resource SQLSTATE classes 08/53/57/58 — class 57 includes 57014 (`query_canceled` / statement_timeout expiry); all map to 503 so clients retry instead of treating a transient timeout as a code bug |
| `token_service_unavailable` | 503 | Additive — Token Service TS-1/TS-4 unreachable (§2.5) |
| `request_timeout` | 503 | Additive — Postgres statement/lock timeout (57014/55P03); retryable, not an outage |
| `tenant_mismatch` | 403 | Additive — `RequireTenantMatch`: the `/tenants/:id` (or provision body) tenant is not the caller's `x-tenant-id`; rejected before any Keycloak call |

`Error.Status()` derives the HTTP code from `Code` via a lookup table; an unrecognized code defaults to 500. `Error.WithDetails(map[string]any)` attaches structured context echoed into the response body's `details` field.

## Events

### Produced — `iam-tenant-events` SNS topic, 9 event types (§25, §7.5)

Envelope `source = "iam-realm-provisioner"`. Cron-originated events (RP-C1/RP-C3) use the sentinel actor `SystemActorID = "00000000-0000-0000-0000-0000000000a1"`.

| Event | Producer | Key payload fields |
|---|---|---|
| `TenantIdentityProvisioned` | RP-1, RP-2 | `tenant_id`, `owner_user_id`, `slug`, `name`, `plan`, `signup_type` (`trial`\|`direct_paid`), `realm`, `realm_type` (`shared`\|`dedicated`) — BFF-2 signup backstop: O&M runs I-1 if the tenant is missing (RP-D16) |
| `TenantRealmReady` | RP-2, RP-3 | `tenant_id`, `realm`, `keycloak_shard`, `oidc_clients[]` |
| `TenantConverted` | RP-2 (`direct_signup:true`), RP-3 (`direct_signup:false`) | `tenant_id`, `realm`, `plan`, `billing_subscription_id?`, `users_migrated`, `direct_signup` |
| `DirectPaidSignup` | RP-2 | `tenant_id`, `realm`, `direct_signup` |
| `TrialExpired` | RP-C1 (sweep) / RP-12 (direct call) | `tenant_id`, `trial_ends_at`, `grace_ends_at` |
| `TrialReactivated` | RP-4 (revert), RP-10 (shared-strategy reactivate) | `tenant_id`, `new_trial_ends_at` |
| `TenantSuspended` | RP-C3 (`source:"billing_lapse"`), RP-14 (`source:"operator"`) | `tenant_id`, `realm`, `cancelled_at?`, `suspended_at`, `source`, `reason?` |
| `TenantOffboarded` | RP-C3 (post-export-confirmed delete) | `tenant_id`, `realm`, `export_s3_key`, `offboarded_at` |
| `TenantReactivated` | RP-10 (dedicated-strategy reactivate) | `tenant_id`, `realm`, `source`, `reason?` |

Every payload is validated against an embedded JSON Schema (`internal/eventschema/*.json`, `ByEventType`) at outbox-enqueue time (shape only — wire encoding via the Glue codec happens later, at SNS-publish time, with the version UUID `GlueCodec` resolved once at startup by definition — `glue:GetSchemaByDefinition`, never "latest", no refresher). `internal/eventschema/asyncapi_drift_test.go` asserts `api/asyncapi.yaml`'s `iamTenantEvents` channel names exactly these 9 events and no others.

### Consumed — 3 SQS queues, 5 distinct event types (§7.1)

| Queue | `EventType` filter | Source topic | Drives |
|---|---|---|---|
| `membership-realm-q` | `DepartmentMembershipGranted`, `DepartmentMembershipLevelChanged`, `TenantRoleGranted`, `TenantRoleRevoked` | O&M's `iam.membership.events` | `MfaService.SyncRequiresMfa` — grant path unconditional assign-if-absent; revoke path (`TenantRoleRevoked` only) consults O&M's I-8 residual-MFA-requirement check before revoking |
| `tenant-realm-q` | `TrialStarted` | O&M's `iam.tenant.events` | Idempotent upsert of `trial_ends_at` onto the tenant's `tenant_realms` row — this service never queries O&M for trial timing, it's push-only |
| `billing-realm-q` | `TenantReactivated` | Billing's `billing.events` (same event *name* as this service's own produced `TenantReactivated`, but a different topic/producer — no self-consumption) | `LifecycleService`-equivalent reactivation — re-enables a `suspended` dedicated realm only; no-op otherwise (still recorded in `processed_events`) |

Each queue has a `-dlq` sibling, `maxReceiveCount=5`. Dedup via `processed_events` PK `(event_id, consumer)`, `consumer ∈ {membership, tenant, billing}`.

**Downstream consumers of this service's `iam.tenant.events` (CD-26 — not consumed by this service):**

| Queue | Owner | `EventType` filter |
|---|---|---|
| `tenant-billing-q` | Billing Service | `TenantSuspended`, `TenantReactivated` (CD-24: operator suspensions + un-suspends previously invisible to Billing) |
| `tenant-entitlements-q` | Entitlements Service | `TenantConverted`, `TenantSuspended`, `TenantReactivated`, `TenantOffboarded`, `TrialExpired` (CD-6) |
| `tenant-usage-q` | Usage & Metering Service | `TenantConverted`, `TenantSuspended`, `TenantOffboarded` |

These are documented in `api/asyncapi.yaml` (three `sqsTenant*` server entries) and the LLD §7.4 consumer table for cross-team visibility. Each has a `-dlq` sibling, `maxReceiveCount=5`.

**Inbound pipeline** (`internal/adapter/inbound/sqspipeline`: `NewConsumer` from `eventcfg.LoadSQS` + `EnvForQueue`, `Handler`; `RequireDLQ` fails startup for a queue with no RedrivePolicy): the library's `GlueDecodeCodec` strips the Glue header → straight-to-DLQ route → consumer span → `validateConsumed` → `Handle` (platform-events records `platform_messages_received/processed/failed_total{queue,event_type}` around the handler). `validateConsumed` checks the payload against the consumer's own consumed-message schema (`eventschema.ConsumedByConsumer` — 6 schemas, per consumer, so billing's `TenantReactivated` uses `billing_tenant_reactivated.json`, never the produced one); unknown types pass to `ackUnknown` (`unknown_event_acknowledged_total{topic=<source topic>,event_type}`); a violation increments `iam_realm_provisioner_consumed_schema_violations_total{consumer,event_type}` (critical alert) and is sent straight to `-dlq` on its first delivery (`SendToDLQ`, original body and attributes, reason `schema_violation`). Persistently failing messages reach the handler on deliveries 1–4; platform-events forwards the 5th to `-dlq` itself (`WithDLQForwarding`, `platform_dlq_messages_total`), before SQS's own redrive. Both go through the library's DLQ publisher, so arch-lint's events invariant holds. Dedup: platform-events' `pkg/inbox` over `processed_events`.

### AsyncAPI & Schema Drift

`api/asyncapi.yaml` — design-time source of truth, embedded via `api/embed.go`, served live at `/asyncapi` and `/asyncapi.yaml` (gated by `DOCS_ENABLED`/`DOCS_AUTH_TOKEN` in production). Declares 7 servers (the SNS topic + 3 inbound SQS queues + 3 downstream Commercial consumer SQS queue references for cross-team visibility, CD-26), 4 channels, and per-message schemas. Two complementary schema-governance layers now exist: `internal/eventschema/asyncapi_drift_test.go` is a fast, no-AWS CI check — it structurally parses the `iamTenantEvents` channel's `messages:` block out of the YAML and asserts its key set equals `eventschema.ByEventType`'s map keys exactly — and `.github/workflows/schema-registry.yml`/`schema-prune.yml`/`schema-health-quarterly.yml` (the `platform-schemagov` CLI) validate/diff/register this service's 9 schemas against the shared `iam-tenant-events` Glue registry for real. See `docs/runbook-schema-registry.md` and `.claude/operations.md` §13.2.1.
