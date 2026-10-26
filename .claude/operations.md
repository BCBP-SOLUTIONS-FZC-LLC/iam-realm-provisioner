# Operations

## 10. Security

### 10.1 Keycloak Admin Credential (§10.2 — "the crown jewel")

The Keycloak admin credential is an OpenBao secret, surfaced via External Secrets Operator as an ephemeral volume — **never** an env var, image layer, or git artifact. RP-INV-1 (this service is the *only* holder of a Keycloak admin credential in the entire platform) means only this service's OpenBao role can `read` that path. The admin access token itself is cached only in Valkey/in-process, never logged, never persisted to Postgres, with the shortest practical lifetime.

**Keycloak admin role set:**
- Master realm: `create-realm` **only** — not full master `admin`. Required for `POST /admin/realms`.
- Managed realms: `realm-admin` composite (covers `manage-realm`/`manage-users`/`manage-clients`/`manage-identity-providers`) — covers realm enable/disable, client registration, user CRUD (RP-15), IdP config (RP-6), realm-role assign/revoke, credential reset (RP-9), session/token revocation (RP-16).
- `create-realm` auto-confers admin on realms this account creates — no per-realm bootstrap hook needed since RP is the sole creator (RP-INV-1).
- Cross-realm migration (trial→dedicated, RP-3) — one account holds rights on both the source realm (`view-users`/`query-users` via `realm-admin` on `trial`) and the target realm (`manage-users`/`manage-realm` via `realm-admin` on the new realm).
- Deferred hardening (not MVP): split bootstrap (`create-realm` only) vs runtime (`realm-admin`) accounts.

### 10.2 Per-Tenant IdP Secrets

Dynamic and unbounded, so accessed via the OpenBao KV API at runtime (not ESO). Path: `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>` — fully derivable from `(tenant_id, idp_alias)` already on the `keycloak_idp_configs` row, so **no reference/pointer to it is ever stored in Postgres**. OpenBao policy (Kubernetes auth: pod ServiceAccount token → OpenBao role → policy) grants `create`/`read`/`update`/`delete` on `secret/data/iam/realm-provisioner/idp/*` plus `read` on the Keycloak-admin path only — nothing else. The secret passes through the RP-6 HTTP handler transiently, straight to OpenBao then Keycloak, never persisted or echoed back. Keycloak also keeps its own encrypted copy (for federated-login-time use); OpenBao lets this service re-apply/rotate the secret without customer re-submission. RP-7 (remove IdP) deletes **all** copies — Keycloak IdP config and the OpenBao KV entry.

### 10.3 Network Isolation

All routes under `/api/v1/internal/*` except `/healthz`/`/readyz`/`/asyncapi`/`/asyncapi.yaml` — mesh-only, mTLS, `RequireSystemRole` defense-in-depth (NetworkPolicy + mesh mTLS is primary). `/metrics` is served on its own dedicated port (`METRICS_PORT=9090`), never sharing a listener with the provisioning API, and is monitoring-namespace-only.

### 10.4 Input Validation

`strategy` ∈ `{shared, dedicated}`; `protocol` ∈ `{saml, oidc}`; realm-naming slug charset/length checked; `Idempotency-Key` non-empty, ≤256 chars; IdP `config` validated by the §11.6 checklist **before** any Keycloak write (RP-INV-4); `setting_key` (RP-8) allow-listed, else `400 unsupported_realm_setting`. No secret material is ever accepted into or stored in Postgres.

### 10.5 Authorization Matrix

| Action | Required roles |
|---|---|
| RP-1..RP-10, RP-12, RP-15, RP-16 (provisioning/lifecycle/IdP/MFA/user ops) | Internal system principal only (mesh peer, NetworkPolicy) |
| RP-11 (read realm status) | Internal system principal, incl. O&M/operator tooling |
| RP-13 (whole-estate IdP portability audit) | `platform_operator` only |
| RP-14 (emergency suspend) | `platform_operator` only |
| RP-3, RP-4, RP-11 (CD-19) | Also `commercial-system` (Billing). It reaches no other route, at all three layers: Istio, NetworkPolicy and `RequireSystemOrCommercial`. CI asserts this on the staging/prod renders (`check-cd19-access-matrix.py`, §13.2) and in `TestCD19_CommercialSystem_RouteMatrix` |

RP makes **no** end-user authorization decision — that is AuthZ Enrichment's job. Callers are trusted mesh peers (internal-route guard + mTLS); the `x-tenant-id` header scopes the request and RLS enforces isolation. RP-13/RP-14 additionally require an operator role in `x-tenant-roles`/the platform-admin claim, re-checked at the handler (defense-in-depth, network isolation is primary).

### 10.6 Platform-Automation Service-Account Client (§2.5, RP-17/RP-18, EXT-6)

Distinct from the Keycloak admin credential above: this is the per-tenant `platform-automation` Keycloak client every tenant gets, the identity the Tender Management automation surface authenticates as. It is **not** secured by a Keycloak client-secret — an earlier design (pre-EXT-6) minted it that way and had RP-17 apply Token Service's issued secret directly via `UpdateClient{Secret:…}`, but that was proven wrong against a real Keycloak 26.0 instance (one secret per client, no dual-secret grace window, so no genuine rotation overlap was ever possible). The corrected, current design: the client is minted with `ClientAuthenticatorType: "client-jwt"` (private_key_jwt) and `attributes.jwks.url` pointing at a JWKS Token Service serves per tenant — Keycloak validates a signed assertion against whatever public keys that JWKS currently publishes, so two keys (old + new) can be live simultaneously for genuine rotation overlap. This service never holds, transmits, or stores the private key material at any point: Token Service generates it, custodies it in its own OpenBao, and serves only the public half. RP-17 (`ClearServiceAccountKeysCache`) exists only because Keycloak does not self-refresh a `jwks.url` client's cached keys — a newly-added key is unrecognized and a removed key keeps authenticating until this is explicitly called; a client's first key needs no such warm-up.

**The JWKS fetch endpoint is intentionally unauthenticated — not a gap.** Token Service serves `GET .../tenants/:id/service-accounts/platform-automation/jwks.json` outside its own protected-route middleware group, since Keycloak's outbound fetch carries no caller identity (no `x-user-id`/`x-tenant-id` headers) and there is no way to make it carry one. This is a deliberate, documented exception on the Token Service side, not something this service's own `RequireSystemRole`/mesh-mTLS conventions apply to (it isn't this service's route).

### 10.7 §11.6 Region-Agnostic IdP Configuration Checklist

Checks every SAML/OIDC config field against a fixed list of AWS region substrings and against the one platform-wide region-neutral auth host (`auth.bcbpsolutions.com`).

- **CRITICAL (block, `400 idp_region_portability_failed`), 12 total:** no config value contains an HTML/XML injection sequence; SAML SSO/SLO URL hostnames carry no AWS-region substring; a customer-provided signing cert is present; ACS/SLO/EntityID point at the region-neutral host; OIDC issuer/discovery is region-neutral; OIDC redirect URIs are region-neutral; OIDC audience matches the region-neutral client ID; an OIDC `client_id` is present; an OIDC `issuer` is a non-empty, valid HTTPS URL.
- **WARN (allow with a recorded acknowledgement, else `422 idp_warnings_unacknowledged`):** SAML `NameIDFormat != "transient"`; custom attribute-mapper URNs are region-free; SAML signature algorithm ≥ `RSA-SHA256`; OIDC PKCE `S256`. Acknowledgement recorded with the admin's `user_id` + timestamp in `keycloak_idp_config_validations.acknowledgements`.
- RP-13's whole-estate audit re-runs the same `ValidateIdpConfig` function against every stored config's non-secret `Config` (never re-reading secrets from OpenBao) to compute `{total_tenants, total_idp_configs, passing, warnings_only, failing, failing_tenants[]}`.

## 11. Observability

Implementation docs: `docs/observability/` — `README.md` (identity, tiers, who-counts-what, CI), `metric-registry.md` (generated by `make metrics-inventory`; `make metrics-lint` fails when stale), `migration.md`, `runbooks.md` (one `###` entry per alert; `TestMonitoring_EveryAlertHasARunbook` enforces the `runbook` annotation ↔ heading match).

### 11.1 Metrics

Instruments follow the Enterprise Platform Observability Standard's three-tier hierarchy. The central **Platform Observability Registry** (platform-gincommon's `pkg/obsregistry`) is the single source of truth for every shared name, label set and label vocabulary; there is no local copy. Every series carries `{domain="iam", service="iam-realm-provisioner", environment=<APP_ENV>}`, injected centrally by gincommon (`gincommon.Config.Domain`/`Environment`, else `OBSERVABILITY_DOMAIN`/`APP_ENV`) and passed to platform-events and platform-pgcommon from `gincommon.MetricsConstLabels()`. `APP_ENV` must be one of `local|dev|test|staging|prod` (no aliases: `production` refuses to start). Only `platform_build_info` carries `version`.

- **Tier 1 (`platform_*`)** — emitted by the platform libraries (HTTP: platform-gincommon; messaging/outbox/SNS/SQS: platform-events; DB: platform-pgcommon), plus one this service records itself (below). Several are still registry status `proposed` (awaiting governance); with the pre-standard names removed they are the only source for those signals, so every alert reading one carries `metric_status: proposed`.
- **Tier 2 (`iam_*`, domain-shared)** — none currently apply. RP-INV-1 (this service is the sole Keycloak Admin API caller in the platform) means almost every metric here represents a behavior no other IAM service can perform, so it fails the "shared across IAM services" test and stays Tier 3.
- **Tier 3 (`iam_realm_provisioner_*`, frozen §25 name inventory, = the registry's `<domain>_<service>_` prefix)** — everything else.

**Tier 1 recorded by this service:**

| Metric | Type | Labels | Description | Tier 3 counterpart |
|---|---|---|---|---|
| `platform_dependency_request_seconds` (proposed) | Histogram | `dependency="keycloak"`, `operation` (the frozen §25 op names), `outcome` (`success`\|`error`) | One sample per Keycloak Admin API call. Built from its registry entry (`gincommon.NewSharedHistogramVec`) before `events.InitMetrics`, so platform-events records its `sns`/`sqs`/`codec` calls on the same collector. | `iam_realm_provisioner_keycloak_call_duration_seconds` + `_keycloak_call_failures_total` |

Message-lifecycle metrics (`platform_messages_received/processed/failed_total{queue,event_type,reason}`, `platform_dlq_messages_total{operation,event_type,reason}`) come from platform-events' SQS consumer, by queue name — this service no longer emits its own per-`consumer` copies. Dead-lettering is platform-events' own: `WithDLQForwarding` (max-receive 4, one short of the queue's `maxReceiveCount=5`) forwards a persistently failing message on its 5th delivery, and `sqspipeline`'s straight-to-DLQ route forwards a consumed-schema violation on its first — both counted in `platform_dlq_messages_total{operation="consume",reason}`. Dedup is platform-events' `pkg/inbox`: it records `platform_duplicate_messages_total{queue,event_type}`; this service's own `iam_realm_provisioner_processed_events_duplicates_total{consumer}` stays the Tier 3 signal. There is no platform-wide RLS metric in the registry, so RLS-6 is Tier 3 only.

**Tier 3 — `iam_realm_provisioner_*` (authoritative):**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `iam_realm_provisioner_provision_total` | Counter | `kind` (`trial`\|`direct`\|`convert`), `result` | Provisioning attempts |
| `iam_realm_provisioner_users_migrated_total` | Counter | — | Users migrated across a conversion |
| `iam_realm_provisioner_realm_active_gauge` | Gauge | `strategy` | Active realms by strategy |
| `iam_realm_provisioner_keycloak_call_duration_seconds` | Histogram | `op` | Keycloak Admin API call latency — every alert/SLO reads this (see Governance above) |
| `iam_realm_provisioner_keycloak_call_failures_total` | Counter | `op` | Keycloak Admin API call failures — every alert/SLO reads this |
| `iam_realm_provisioner_admin_token_refresh_total` | Counter | `trigger` (`proactive`\|`401`) | Admin token cache refresh — a `401`-triggered spike signals credential rotation, not an outage |
| `iam_realm_provisioner_idp_validation_total` | Counter | `result` | §11.6 validation run outcomes |
| `iam_realm_provisioner_mfa_role_sync_total` | Counter | `action` (`assign`\|`revoke`) | `membership-realm-q` MFA role reconciles |
| `iam_realm_provisioner_realm_config_pending_gauge` | Gauge | — | `count(*) WHERE status='pending'` in `realm_config_reconciliations` |
| `iam_realm_provisioner_trial_expiry_processed_total` | Counter | — | RP-C1 rows processed |
| `iam_realm_provisioner_trial_expiry_deferred_total` | Counter | — | RP-C1 rows deferred — **must increment on every defer**, not just be registered at startup, or the "sustained >0" alert can never fire |
| `iam_realm_provisioner_orphaned_signup_reap_total` | Counter | `result` (`reaped`\|`skipped`\|`deferred`) | `orphaned-signup-reap` per-tenant outcomes (pushed via the Pushgateway, `cronjob="orphaned-signup-reap"`) |
| `iam_realm_provisioner_lapse_processed_total` | Counter | `transition` (`suspend`\|`offboard`) | RP-C3 transitions |
| `iam_realm_provisioner_realm_export_total` | Counter | `result` | Offboarding export attempts (RP-C3) |
| `iam_realm_provisioner_user_delete_total` | Counter | `result` | RP-15 |
| `iam_realm_provisioner_session_revoke_total` | Counter | `result` | RP-16 |
| `iam_realm_provisioner_mfa_reset_total` | Counter | `result` | RP-9 (fail-closed semantics) |
| `iam_realm_provisioner_audit_entry_delivery_total` | Counter | `result` (`delivered`\|`retry`\|`unknown_entry_type`\|`rejected`) | AL-5 `TenantIdpConfigChanged`/`TermsAccepted` delivery attempts (`unknown_entry_type`: Audit Log not yet at rev 0.31 — row kept and retried) from `pending_audit_entries` — sustained `retry` = Audit Log unreachable; any `rejected` = an entry Audit will never accept (inspect the `failed` row's `last_error`) |
| `iam_realm_provisioner_audit_entry_backlog_gauge` | Gauge | `status` (`pending`\|`failed`) | `pending_audit_entries` row count, refreshed by the 5-min exporter — growing `pending` with no `delivered` means Audit Log is down or `AUDIT_LOG_BASE_URL` is unset |
| `iam_realm_provisioner_template_drift` | Gauge | `realm_version` | Report-only realm-template drift (RP-3) |
| `iam_realm_provisioner_rls_violations_total` | Counter | `violation_type` | RLS-6 violations — every RLS alert reads this (no platform-wide RLS metric exists in the registry) |
| `iam_realm_provisioner_unknown_event_acknowledged_total` | Counter | `topic`, `event_type` | Inbound events silently ack'd because no handler is wired — the standard's own worked Tier 3 example, so not a candidate for promotion |
| `iam_realm_provisioner_consumed_schema_violations_total` | Counter | `consumer`, `event_type` | Inbound deliveries rejected before the handler because the payload failed its consumed-message schema (`eventschema.ConsumedByConsumer`, `cmd/server/inbound_schema.go`) — counted once per delivery; SQS retries until redrive to `-dlq`. Pages via `IAMRealmProvisionerConsumedSchemaViolation`. |
| `iam_realm_provisioner_processed_events_duplicates_total` | Counter | `consumer` | SQS redeliveries filtered by dedup |

Plus the Tier 1 metrics of the libraries: `platform_http_*` and `platform_build_info` (platform-gincommon), `platform_messages_*`, `platform_dlq_messages_total`, `platform_outbox_*`, `platform_dependency_request_seconds{dependency="sns"|"sqs"|"codec"}` and `platform_library_info` (platform-events, `events.InitMetrics`), and `platform_db_*` query/pool/retry instruments (platform-pgcommon, `pgmetrics.InitWithIdentity` — called before `NewPool`, which registers the pool gauges). All land on the registry `gincommon.MetricsRegisterer()` returns — `cmd/server/main.go` runs `ObservabilityMiddlewares` first (gincommon's metrics-init API), then `metrics.Register()`, then `initLibraryMetrics` (logs every `RegistrationWarning`, fails on an invalid identity), rather than assuming `DefaultRegisterer`. No projection-lag metric — this service is a producer, not a projector.

CI enforcement: `internal/adapter/outbound/metrics/metrics_test.go`'s `TestNamingConvention_ConformsToPlatformObservabilityStandard` validates every gathered collector with `obsregistry.Validate` (names, suffixes, required identity labels, the `iam_realm_provisioner_` prefix, label vocabularies, prohibited labels, removed names), run by `make test-unit`/`make ci` and as its own `validate-quality.yml` step (`make metric-naming-lint` → `.github/scripts/check-metric-naming.sh`). `.github/scripts/metrics-registry-lint.sh` (`make metrics-registry-lint`, in `make ci` and `validate-test.yml`) runs `rules_test.go`: no removed metric name in the rule/HPA/adapter files (even in comments), and every alert reading a `proposed` metric carries `metric_status: proposed` (and no other alert does).

> **DB-state gauges.** `realm_active_gauge`, `realm_config_pending_gauge`, and `template_drift` are refreshed every 5 minutes by exporter goroutines in `cmd/server` (`runBusinessExporters` → `pgadapter.GaugeRepository`, querying the BYPASSRLS sysPool), emitting once at startup so they are populated before the first scrape. A failing query logs `gauge exporter query failed` at WARN and keeps the previous value, so **a flat gauge plus that log line means the exporter is broken, not that the count is stable** — check the log before trusting a flat line. `template_drift` series disappear when a version stops drifting (the vec is reset each poll); `realm_active_gauge` reports an explicit `0` instead.
>
> **Not-yet-armed instruments.** `trial_expiry_*`, `lapse_processed_total`, and `realm_export_total` are incremented correctly but only inside the reconciler CronJob, which exits before a scrape lands and exposes no `/metrics` listener; exporting them needs a Pushgateway or DB-backed state the server reads (the sibling `iam-user-profile` reconciler has the same limitation). Treat silence on these as "no signal", not "no problem". See LLD §11.2.

**Alerts (`deploy/monitoring/app-alerts.yml`):**
- `IAMRealmProvisionerDown` — no healthy instances for 2m → **critical** (blocks all 17 endpoints + all 3 SQS consumers; the reconciler CronJobs run as separate pods and are not covered)
- `IAMRealmProvisionerReplicasMissing` — fewer than 2 healthy replicas for 5m → warning
- `IAMRealmProvisionerHighErrorRate` — `platform_http_requests_total{status_class="5xx"}` ratio > 5% for 5m, per `{domain, service, environment}` → warning (`metric_status: proposed`)
- `IAMRealmProvisionerCriticalErrorRate` — 5xx rate > 20% for 2m → **critical** — escalate immediately, RP-INV-1's single Keycloak writer means every tenant realm operation funnels through this one service
- `keycloak_call_failures_total` rate > 5% for 5m → warning
- `trial_expiry_deferred_total > 0` sustained 1h → warning
- `realm_config_pending_gauge > 0` sustained 30m → warning (RP-INV-6 risk — stored intent and actual Keycloak state have diverged)
- `realm_export_total{result="fail"} > 0` → **critical** (compliance-blocking)
- `platform_dlq_messages_total{operation="outbox_publish",reason="max_attempts"}` increase > 0 over 30m (incl. series born at 1) → warning
- `platform_dependency_request_seconds_count{dependency="sqs",operation="receive_message",outcome="error"}` increase > 0 over 30m → warning (`metric_status: proposed`; no queue label)
- `{membership,billing,tenant}-realm-q` DLQ depth > 0 → warning
- `admin_token_refresh_total{trigger="401"}` spike → possible credential-rotation issue, not necessarily an outage
- RLS-6 cross-tenant access / missing-or-invalid-GUC → critical (no `for`) / warning (10m) — reads `iam_realm_provisioner_rls_violations_total{violation_type=...}` (Tier 3)

### 11.2 Tracing

`gincommon.InitTracingWithConfig()` in both binaries, with the same identity as the metrics (always — in-process spans get valid trace IDs even without a collector; `OTEL_EXPORTER_OTLP_ENDPOINT=none` / `OTEL_TRACES_EXPORTER=none` disables export), plus `pgcommon.Config.Tracer = gincommon.NewSpanTracer(...)` for a `db.query` span per query. W3C `traceparent` is injected on every outbound HTTP call (O&M, OpenBao, Keycloak) by the shared `adapter/outbound/httpx` transport; SNS instead carries `trace_id` on the outbox envelope, so provision → event → O&M-projection is one trace. Keycloak spans are named `keycloak.<op>` (e.g. `keycloak.create_realm`, `keycloak.migrate_user`); a CronJob run is rooted at `reconciler.<job>`.

### 11.3 Structured Logs

Fields: `tenant_id`, `operation`, `idempotency_key`, `keycloak_realm`, `actor`. **Never** log the admin token, password seed, or an IdP secret.

### 11.4 Dashboards

Grafana "IAM — Realm Provisioner": provision throughput/success by kind, conversion duration + users-migrated, Keycloak call latency/error by op, admin-token refresh rate, IdP validation outcomes, MFA-role sync rate, realm-config pending gauge, trial-expiry/lapse processed+deferred, realm-export success, outbox/SQS/DLQ depths.

## 12. Configuration

| Variable | Default | Description |
|---|---|---|
| `APP_ENV`, `APP_PORT` | `dev` / `8080` | Environment; HTTP port |
| `METRICS_PORT` | `9090` | Dedicated `/metrics` listener, never shared with the API |
| `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` | — | Postgres DSN parts for `provisioner` |
| `PG_MAX_CONNS`, `PG_BOUNCER_MODE`, `PG_STATEMENT_TIMEOUT`, `PG_SLOW_QUERY_THRESHOLD` | `10` / `false` / `5s` / `200ms` | `pgcommon` pool tuning |
| `SYSTEM_DATABASE_URL` | (required) | `realm_provisioner_migrator` (`BYPASSRLS`) DSN — the reconciler's cross-tenant sweeps use this, never the app role. **Required outside dev** — both `cmd/server` and `cmd/reconciler` panic at startup if absent in non-dev environments; without it, cross-tenant sweeps are RLS-filtered to zero rows and GDPR hard-delete is disabled. |
| `MIGRATION_DATABASE_URL` | (required) | Direct (non-PgBouncer) DSN — only `realm_provisioner_migrator` can run `outbox.ApplySchema` / the domain migration; `realm_provisioner_app` has no `CREATE` on schema `public` |
| `AWS_REGION`, `AWS_ENDPOINT_URL` | `ap-south-1` / — | Region + Floci endpoint override for local dev |
| `SNS_TOPIC_TENANT_ARN` | (required in prod; Helm **secret**) | Single outbound topic, `iam-tenant-events`. In Helm's `envFromSecret`, as in iam-org-membership, so the chart refuses to render without it |
| `GLUE_REGISTRY_TENANT_NAME` | `iam-tenant-events` (required in prod) | Unset → `NoopCodec` (plain JSON, local dev) |
| `SQS_MEMBERSHIP_REALM_QUEUE_URL` / `SQS_TENANT_REALM_QUEUE_URL` / `SQS_BILLING_REALM_QUEUE_URL` | (required in prod; Helm **secret**) | The 3 inbound queues — an empty URL silently disables that consumer (WARN), so Helm keeps them in `envFromSecret` |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_DRAIN_TIMEOUT` / `OUTBOX_RETENTION_DAYS` | `500ms` / `50` / `5` / `30s` / `8` | Outbox runner tunables, read by platform-events' `eventcfg.LoadOutbox` (+ `RunnerConfigFromEnv`) |
| `OUTBOX_PUBLISH_CONCURRENCY` / `OUTBOX_PUBLISH_TIMEOUT` / `OUTBOX_STARTUP_JITTER` / `OUTBOX_CLAIM_LEASE_DURATION` | `4` / `10s` / `2s` / `10m` | Pinned in Helm/.env-example — `eventcfg`'s library defaults (1 / 10s / 0 / 0) apply only when unset |
| `SQS_MAX_RECEIVE_COUNT` / `SQS_VISIBILITY_TIMEOUT` / `SQS_WAIT_SECONDS` / `SQS_MAX_MESSAGES` / `SQS_HANDLER_TIMEOUT` / `SQS_DRAIN_TIMEOUT` / `SQS_QUEUE_DEPTH_INTERVAL` | `4` / `30s` / `20` / `10` / `20s` / `15s` / `60s` | Shared consumer tuning for all three queues (`eventcfg.LoadSQS`, defaults filled by `sqspipeline.EnvForQueue`). Max-receive must stay below the queues' RedrivePolicy `maxReceiveCount=5` so platform-events' DLQ forward wins over SQS's silent redrive |
| `RUN_MIGRATIONS` | `true` locally, `false` on every Kubernetes replica | `false` = the migrate hook Job (`cmd/migrate`, Helm `migrate-job.yaml`, `migrations.job.enabled`) owns the schema |
| `MIGRATE_TIMEOUT` | `10m` | `cmd/migrate` whole-run bound, lock wait included; below the Job's `activeDeadlineSeconds` (900) |
| `PROMETHEUS_PUSHGATEWAY_URL` | (unset) | Reconciler CronJobs push each run's metrics here (grouped by `cronjob`); unset = reconciler counters are never exported |
| `LOG_LEVEL` / `LOG_SAMPLING` | `info` / `false` | platform-gincommon logger (all binaries) |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | Consumer dedup retention (must exceed the 7-day SQS message lifetime) |
| `KEYCLOAK_BASE_URL` | (required) | e.g. `https://auth.bcbpsolutions.com` — fail-fast on the dev default outside `APP_ENV=dev` |
| `KEYCLOAK_ADMIN_REALM` | `master` | |
| `KEYCLOAK_ADMIN_CLIENT_ID` / `KEYCLOAK_ADMIN_CLIENT_SECRET` | `iam-realm-provisioner` / (from OpenBao via ESO) | Fail-fast on empty/dev-default outside `APP_ENV=dev`. The secret is read fresh from env at each token refresh (`Config.SecretProvider`) so an ESO-rotated secret takes effect on the next pod restart without rebuilding the adapter. Future: wire `SecretProvider` to a live OpenBao reader to eliminate the restart requirement. |
| `KEYCLOAK_RATE_LIMIT_RPS` | `20` | Client-side rate limiter — absorbs bursts during mass migration |
| `KEYCLOAK_TIMEOUT_MS` | `8000` (as `8s` in `.env-example`) | Per-operation timeout budget |
| `BAO_ADDR` | e.g. `https://openbao.iam.svc.cluster.local:8200` | OpenBao — Kubernetes auth (pod ServiceAccount token) |
| `BAO_AUTH_ROLE` / `BAO_KV_MOUNT` / `BAO_IDP_PATH_PREFIX` / `BAO_KEYCLOAK_ADMIN_PATH` | `iam-realm-provisioner` / `secret` / `iam/realm-provisioner/idp` / `iam/realm-provisioner/keycloak-admin` | |
| `SHARED_TRIAL_REALM` / `REALM_TEMPLATE_VERSION` | `trial` / `v1` | Shared trial realm name + active dedicated-realm template version |
| `IDEMPOTENCY_IN_PROGRESS_LEASE` | `15m` | An `in_progress` ledger row older than this is reclaimed by the next retry (crashed/SIGKILLed execution). Must exceed `CONVERT_TIMEOUT` |
| `CONVERT_TIMEOUT` | `10m` | RP-3 conversion runs detached from the 30s request deadline (caller gets `409 operation_in_progress` and retries with the same key); this bounds it |
| `PG_LOCK_TIMEOUT` | `2s` | pgcommon per-transaction `lock_timeout`; a fire maps to `503 request_timeout` |
| `PROVISIONING_OPERATIONS_TTL_DAYS` / `RLS_VIOLATION_LOG_TTL_DAYS` | `30` / `90` | `ledger-prune` CronJob retention windows |
| `REALM_TEMPLATE_PATH` | `/deploy/keycloak/realm-template.json` (Helm) | The versioned realm template the image ships (§12.2). Unset → the copy embedded in the binary, silently |
| `ORG_MEMBERSHIP_BASE_URL` | (required) | O&M client (I-8 residual-MFA check) — fail-fast on the dev default outside `APP_ENV=dev` |
| `ORG_MEMBERSHIP_TIMEOUT_MS` | `5s` | O&M HTTP client per-request timeout. Value is a Go `time.ParseDuration` string (e.g. `5s`, `30s`) — the `_MS` suffix in the name is legacy; a bare integer parses as an error and silently falls back to the default |
| `TOKEN_SERVICE_BASE_URL` | `http://iam-token-service.iam.svc.cluster.local` (Helm; required) | Token Service client (TS-1/TS-4 mint/rotate handshake, §2.5) — required for every RP-1/RP-2/RP-3/RP-4 call and RP-17; no optional-degrade path (unlike `ORG_MEMBERSHIP_BASE_URL`'s reconciler-only usage, §20); fail-fast on the dev default outside `APP_ENV=dev` |
| `AUDIT_LOG_BASE_URL` | (unset) | Audit Log client — AL-5 direct-write for `TenantIdpConfigChanged` and the owner's `TermsAccepted` (LLD §10.6/§18.7). **Optional, deliberately not fail-fast**: unset logs a startup WARN and disables the drainer; entries accumulate durably in `pending_audit_entries` and deliver once it is set |
| `AUDIT_DELIVERY_INTERVAL` / `AUDIT_DELIVERY_BATCH_SIZE` | `5s` / `50` | Drainer poll interval + claim batch; a full batch re-drains immediately |
| `REALM_EXPORT_BUCKET` / `REALM_EXPORT_KMS_KEY` | `iam-realm-exports` / `alias/iam-realm-export` | Offboarding export target |
| `TRIAL_GRACE_DAYS` / `CANCELLED_GRACE_DAYS` / `RETENTION_DAYS` | `30` / `30` / `90` | Lifecycle timers |
| `RECONCILER_BATCH_LIMIT` / `RECONCILER_TIMEOUT` / `RECONCILER_MAX_ATTEMPTS` | `100` / `5m` / `20` | Reconciler-job tunables — `RECONCILER_MAX_ATTEMPTS` is RP-C4's poison-pill cap (§13.1) |
| `ORPHAN_SIGNUP_AFTER` | `48h` | `orphaned-signup-reap` window (Go duration, floor `1h`; invalid → that job aborts, others ignore it) |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | `true` / — | AsyncAPI viewer gating — mounted always outside `APP_ENV=production`; opt-in via `DOCS_ENABLED` in prod, optionally bearer-gated |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `none` locally, collector in Helm | platform-gincommon `InitTracingWithConfig` — provider always installed. **Unset is not off**: it falls back to `localhost:4317` and logs failed exports; `none` (or `OTEL_TRACES_EXPORTER=none`) disables export |
| `OTEL_EXPORTER_OTLP_INSECURE` / `OTEL_TRACES_SAMPLER_RATIO` / `OTEL_PROPAGATOR_BAGGAGE` / `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED` | `false` / `0.1` / `false` / `false` (Helm; library prod defaults) | Plaintext gRPC (dev default `true`), head sampling ratio, W3C baggage, re-sample a sampled remote parent (edge services only). `OTEL_SERVICE_NAME` is **not** read — the service name is `APP_NAME` |

**Fail-fast:** startup panics if `KEYCLOAK_BASE_URL` / `KEYCLOAK_ADMIN_CLIENT_SECRET` / `ORG_MEMBERSHIP_BASE_URL` / `TOKEN_SERVICE_BASE_URL` are empty or hold a dev-default value outside `APP_ENV=dev`.

**Realm template versioning:** the in-repo versioned `deploy/keycloak/realm-template.json` (IAM-Platform-owned, PR-reviewed) is embedded/mounted at deploy time, not fetched from S3 or another external source. `REALM_TEMPLATE_VERSION` selects the active version; promotion is CI/CD-gated (dev→staging→prod) by Keycloak import validation + realm-creation smoke tests. A version bump has **no retroactive application** — it only affects newly provisioned realms; there is no bulk re-template job. Drift detection is report-only (no auto-remediation): each realm's `tenant_realms.realm_template_version` is compared against the current `REALM_TEMPLATE_VERSION` and surfaced via `iam_realm_provisioner_template_drift{realm_version}`.

## 13. Deployment & Scaling

**Topology.** EKS namespace `iam` (`ap-south-1`); RDS `provisioner` (Multi-AZ, PgBouncer tx pooling); ElastiCache Valkey (`rp:` keyspace); SNS topic `iam-tenant-events` + 3 consumer SQS queues each with a DLQ; Glue registry `iam-tenant-events`; S3 bucket for realm exports; one Envoy upstream, mesh-internal only.

**Three workloads from one image:** the `server` Deployment (API + 3 SQS consumers + outbox runner + AL-5 audit drainer, `/iam-realm-provisioner-server`, `RUN_MIGRATIONS=false`), the migrate hook Job (`/migrate`, Helm `pre-install,pre-upgrade`), and the `reconciler`, run as 7 K8s CronJobs (`/iam-realm-provisioner-reconciler --job=<jobName>`). The migrate hook runs after the Secret (weight -9) and a hook-scoped NetworkPolicy allowing DNS + Postgres egress (weight -5); it uses the namespace `default` ServiceAccount with no token mounted, because the chart's own ServiceAccount does not exist yet during a first install's pre-install hooks. `terminationGracePeriodSeconds: 75` covers the server's shutdown sequence (parallel 30s drain of HTTP, consumers and in-flight RP-3 conversions, then the outbox, then a 10s pool close).

**Scaling.** `replicaCount: 2` (manual, low-volume). HPA conservative (min 2 / max 4, CPU+memory). The real bottleneck during mass migration is the Keycloak Admin API itself (not this service's CPU) — governed by the client-side rate limiter, not horizontal scaling.

### 13.1 CronJobs

All eight defined in one Helm manifest (`deploy/helm/templates/cronjobs.yaml`), dispatched via `cmd/reconciler/main.go`'s `registry` map (exact `--job=` values below):

| CronJob (`--job=`) | LLD ref | Schedule | Purpose |
|---|---|---|---|
| `trial-expiry` | RP-C1 | `0 2 * * *` | Sweep `tenant_realms WHERE strategy='shared' AND status='active' AND trial_ends_at < now()` (`idx_tenant_realms_trial_exp`) — **never queries O&M**. Calls the idempotent RP-12 effect per tenant, keyed `"trial-expiry:<tenant_id>:<trial_ends_at>"` so a changed `trial_ends_at` naturally produces a fresh key. |
| `trial-hard-delete` | RP-C2 | `0 3 * * *` | For trials past `TRIAL_GRACE_DAYS` (30) beyond expiry: hard-delete users from the shared realm via the Admin API, delete the tenant's `platform-automation-<tenant_id>` client (`DeleteServiceAccountClientIfExists`, idempotent; the Token Service principal is purged by Token Service on O&M's `TenantMembershipsPurged`), atomically delete `keycloak_idp_configs`, `keycloak_idp_config_validations`, `realm_config_reconciliations`, `provisioning_operations` via `NewGDPRScrubFn` (never `pending_audit_entries`: undelivered audit records, LLD v1.41) (`gdpr.go`, BYPASSRLS sysPool tx), then soft-delete the local `tenant_realms` row (retain only `tenant_id` for audit-FK, preserve `sub`). Runs entirely against `SysPool` (`BYPASSRLS`). |
| `orphaned-signup-reap` | §8.7 (BFF gap A-3) | `17 * * * *` (chart's own choice) | Reaps RP-1 signups O&M never created a tenant for (I-1 never succeeded): `strategy='shared' AND status='active' AND deleted_at IS NULL AND trial_ends_at IS NULL AND converted_at IS NULL AND disabled_at IS NULL AND suspension_source IS NULL AND exported_at IS NULL AND created_at < now() - ORPHAN_SIGNUP_AFTER` (48h), oldest first, `RECONCILER_BATCH_LIMIT`. Per tenant, one `SysPool` tx: re-check the predicate `SELECT … FOR UPDATE` (a `TrialStarted` that landed meanwhile → `skipped`), `HardDeleteUsersInRealm`, `DeleteServiceAccountClientIfExists` (automation client), `TokenService.RevokePrincipal` (one TS-7 call, by tenant, even with no recorded principal; a 404 with `errorCode` `PRINCIPAL_NOT_FOUND` = done, any other 404 = TS-7 not deployed = deferred; needs `TOKEN_SERVICE_BASE_URL`, else tenants with a recorded principal are deferred), then `scrubAndTombstoneTenantRealm` (shared with RP-C2). The Token Service principal row stays, `revoked`, with every credential revoked and its material deleted (LLD v1.39). Keycloak failure → rollback, `deferred`. Emits **no** event (O&M has no tenant; trial-hard-delete emits none either); WARN log with `tenant_id` only. Metric `iam_realm_provisioner_orphaned_signup_reap_total{result=reaped\|skipped\|deferred}`. |
| `subscription-lapse` | RP-C3 | `0 2 * * *` | Two-phase suspend→offboard sweep. Suspend polls O&M's I-16 `GET /api/v1/internal/subscription-lapses` (skipped with a WARN if `ORG_MEMBERSHIP_BASE_URL` is unset); offboard always runs against local `suspension_source='billing_lapse'` rows past `RETENTION_DAYS`. Offboard calls `NewGDPRScrubFn` after `DeleteRealmIfExists` — atomically deletes the same four PII tables; hard-fails and never emits `TenantOffboarded` if `GDPRScrubFn` is nil (`SYSTEM_DATABASE_URL` required). |
| `realm-config-reconcile` | RP-C4 | `*/10 * * * *` | Retries every `realm_config_reconciliations` row in `status='pending'`: re-resolve the realm, re-attempt `ApplyRealmConfig`; success → `MarkApplied`, failure → `MarkFailed(lastErr, RECONCILER_MAX_ATTEMPTS)`, which leaves the row pending for the next tick unless its attempt count has just reached `RECONCILER_MAX_ATTEMPTS` (default 20 — poison-pill cap), in which case it moves atomically to the terminal `failed` status instead of retrying forever. |
| `outbox-prune` | — | `0 4 * * *` (this chart's own choice, not LLD-pinned) | Calls `platform-events`' `outbox.Runner.PrunePublished` at `OUTBOX_RETENTION_DAYS` (default 8) retention. |
| `processed-events-prune` | — | `30 4 * * *` (this chart's own choice, not LLD-pinned) | Prunes `processed_events` rows older than `PROCESSED_EVENTS_TTL_DAYS` (default 8). |
| `ledger-prune` | §15 | `0 5 * * *` | Prunes `provisioning_operations` older than `PROVISIONING_OPERATIONS_TTL_DAYS` (30) and `rls_violation_log` older than `RLS_VIOLATION_LOG_TTL_DAYS` (90) via `pgadapter.RetentionRepository` on the `sysPool` — one bounded `DELETE` per transaction (`RECONCILER_BATCH_LIMIT`) until a batch comes back short. Never prunes `keycloak_idp_config_validations` or `failed` `pending_audit_entries`. |

Every job is idempotent per tenant (safe to re-run) and batched (`RECONCILER_BATCH_LIMIT`, default 100). **Instrumentation requirement:** every deferring job must *increment* its `*_deferred_total` counter on every defer, not merely register it at startup — otherwise a "deferred > 0 sustained" alert can never fire even when the job is genuinely stuck.

**Helm chart** (`deploy/helm/`) mirrors sibling charts: `deployment.yaml`, `cronjobs.yaml` (all 8 jobs from one templated range), `migrate-job.yaml` (the migrate hook Job + its hook NetworkPolicy), `hpa.yaml`, `pdb.yaml`, `service.yaml` (+ metrics port), `serviceaccount.yaml` (IRSA annotation), `servicemonitor.yaml`, `prometheusrule.yaml`, `networkpolicy.yaml` (monitoring-namespace scrape + mesh egress to Keycloak/O&M/OpenBao/AWS), `securitypolicy.yaml` (Envoy route CORS/rate limit; `mergeType: StrategicMerge` merges it with the Gateway policy so JWT + ext_authz stay on — Envoy Gateway ≥ v1.8), `secret.yaml` (ESO `ExternalSecret` refs against an OpenBao `ClusterSecretStore`), `httproute.yaml`.

`deploy/iam/` — pod IAM policy for **AWS resources only** (`sns:Publish` to `iam-tenant-events`; `sqs:*` on the 3 queues + DLQs; `glue:GetSchemaByDefinition` (version IDs resolved once at startup by definition — no refresher); `s3:PutObject`/`GetObject` on the export bucket; `kms:*` on the export key). **Secret access is not in this IAM policy** — it is governed entirely by OpenBao.

`deploy/openbao/` — `policy.hcl` (grants on the IdP-secret path prefix + the Keycloak-admin path) + `role.tf.example` (Kubernetes-auth role binding the pod ServiceAccount).

`deploy/keycloak/realm-template.json` — the versioned dedicated-realm template (§12.2 above).

### 13.2 CI/CD (`.github/workflows/`)

- **`validate-test.yml`** — unit + integration (`go test -race`), coverage gate (`.github/scripts/coverage-gate.sh`), Postgres + Keycloak testcontainers, `.go-arch-lint.yml` check plus the no-`gocloak`-outside-`adapter/keycloak` grep (RP-INV-1) and the observability/database/events architectural-invariant greps (`.github/scripts/arch-lint.sh` — RLS-6 is not duplicated here, see `validate-quality.yml` below), and the Enterprise Platform Observability Standard's registry-compliance check (`.github/scripts/metrics-registry-lint.sh` — no unratified `platform_*` metric may source a live alert/recording-rule/SLO, §11.1). A separate **`e2e` job** in the same reusable workflow runs `make test-e2e` in parallel with `test` (as `iam-authz-enrichment` does): the production router over real HTTP against its own Postgres and Keycloak 26 testcontainers, with both images pre-pulled. Callers (`ci.yml`, `release.yml`) gate on both jobs.
- **`validate-quality.yml`** — `golangci-lint`, an inline no-transaction-local-`SET`-GUC check (RLS-6 — matching the sibling IAM services' convention of inlining this gate directly in the workflow rather than a standalone script), the Enterprise Platform Observability Standard's naming/suffix/label/registry-compliance check (`.github/scripts/check-metric-naming.sh`, §11.1), `gofmt`/`go mod tidy`/`go vet`/`govulncheck`, and a Dockerfile digest-pin check. It carries no schema step itself — the AsyncAPI-vs-Glue drift gate (pinned `platform-schemagov` validation of `api/asyncapi.yaml` against the `iam-tenant-events` Glue registry, failing the PR on drift) lives in `schema-registry.yml`'s `pr-check` job instead; see 13.2.1 below for the full schema-governance pipeline that same tool also drives.
- **`validate-helm.yml`** — `.github/scripts/helm-check.sh`: `helm lint`, `helm template` for base / +dev / +staging / +prod, kubeconform, then the **CD-19 access matrix** (`.github/scripts/check-cd19-access-matrix.py`, PyYAML) on the staging and prod renders. It fails if any of the following stops holding:
  - **Istio, Billing:** the principal `<trustDomain>/ns/billing/sa/commercial-billing` gets exactly POST `*/convert`, POST `*/revert-conversion` and GET `*/realm` (RP-3/RP-4/RP-11).
  - **Istio, Signup BFF:** `…/ns/bff/sa/bff-signup` gets exactly POST on the two exact provision paths (RP-1/RP-2).
  - **Istio, namespaces:** the namespace rule is exactly `[iam, observability]` (never `gateway`, `billing`, `commercial` or `bff`), and the policy has no other rule.
  - **NetworkPolicy:** `billing` is admitted only with the pod label `commercial-billing`, and `bff` only with `bff-signup`. Nothing admits `commercial` or every namespace, and there is no namespace-only rule for `bff` or `billing`.
  - **`router.go`:** the `RequireSystemOrCommercial` group registers exactly RP-3/RP-4/RP-11, and the Istio rules, evaluated with Istio's path semantics against every registered route, reach exactly those routes (and RP-1/RP-2 for the BFF).

  The middleware is covered by `TestCD19_CommercialSystem_RouteMatrix`, which calls every `/api/v1/internal` route as `commercial-system`. Run it locally with `make helm-check`. Before this change `validate-helm.yml` was never called from `ci.yml`, so the chart check did not run in CI.
- **`ci.yml`** — orchestrates the above (`validate-helm` gates `push`), plus `build-image` (multi-stage Dockerfile + provenance scripts), a Trivy scan, a smoke check (`/healthz` returns 200, `/asyncapi.yaml` serves a real spec), `pr-summary`, and pushes to GHCR on the default branch.
- **`release.yml`** — semver tag → release binary + image.
- **`changelog-check.yml`** — enforces a `CHANGELOG.md` `[Unreleased]` entry per PR.
- Plus `.github/CODEOWNERS`, `dependabot.yml`, `ISSUE_TEMPLATE/`, `SECURITY.md`, `pull_request_template.md`.

#### 13.2.1 Schema Governance (`schema-*.yml`, `platform-schemagov`)

Three additional workflows govern this service's 9 event schemas on the AWS
Glue Schema Registry `iam-tenant-events` (LLD §7.3.1), all driven by the same
`platform-schemagov` (`schema-gov`) Docker image the drift gate above uses:

- **`schema-registry.yml`** — the registration pipeline. Triggers: PR touching
  `internal/eventschema/*.json`/`api/asyncapi.yaml` (`pr-check` job, read-only
  validate+diff against staging), push to `main` (`staging` job, full
  validate→diff→register→changelog pipeline), a published release
  (`production` job, same pipeline against the production registry), and
  `workflow_dispatch` for either environment. Step order in the full-pipeline
  jobs: validate (8 passes) → check `SCHEMA_FREEZE` → configure AWS
  credentials → detect orphaned schemas (dry-run, informational) → assess
  event usage → enforce usage→lifecycle → diff against the registry (fails on
  breaking changes) → register → update `docs/schema-changelog.md` → emit
  metrics (CloudWatch + Prometheus Pushgateway + GHA annotations).
- **`schema-prune.yml`** — a monthly staging dry-run report plus an
  operator-triggered `workflow_dispatch` execute path (archive to
  `docs/schema-archive/` then `glue:DeleteSchema`). Production pruning is
  never scheduled, only `workflow_dispatch`.
- **`schema-health-quarterly.yml`** — a read-only quarterly GitHub Step
  Summary: lifecycle lint (`schema-gov validate` Pass 4) plus a Glue
  version-accumulation scan, with recommended follow-up actions. Never
  mutates the registry.

**Shared registry with Org & Membership.** `iam-tenant-events` is not this
service's alone — O&M is the topic's other producer and registers two more
schema names there, `TenantCreated` and `TrialStarted` (LLD §7.3.1). This
service's tooling (`make schema-*` and all three workflows above) only ever
acts on the 9 files under `internal/eventschema/`; it never registers,
diffs, verifies, or deletes those two names. Because `schema-gov prune`'s
orphan detection is "Glue schema names minus local file names," those two
names will **always** surface as false-positive "orphans" in a dry-run
report — expected, not a defect. `schema-prune.yml` additionally runs a hard
guard step immediately before any `--execute` invocation that lists the live
registry and fails the job if either name is present, since the underlying
CLI has no `--exclude` flag and `--execute` deletes every candidate it
finds with no way to select a subset. In practice this means automated
`--execute` pruning is unavailable for this service today (those two names
are permanent residents of the shared registry) — see
`docs/runbook-schema-registry.md`'s "Pruning orphaned schemas" section for
the manual fallback.

**Dormant lifecycle enforcement, by design, not by omission.** The
usage-check/enforce-lifecycle steps in `schema-registry.yml` read
`PROMETHEUS_URL` — set, they classify each event's publish activity via
Prometheus and auto-flag a never-fired `active` schema for deprecation (and
a never-fired `deprecated` schema for retirement). `PROMETHEUS_URL` is
deliberately left **unset** in this repo's `.env-example` and in every
environment today: this service is pre-production — nothing is deployed to
any environment and there is no live Prometheus to query (LLD §19.5, the
same "nothing is deployed" fact behind this service's rollback-is-reverting-
the-branch posture) — and its 9 events were frozen at LLD v1.0 sign-off
(§25). Auto-deprecating a frozen
event merely because a pre-production environment has never published it
would be wrong. Leaving `PROMETHEUS_URL` unset makes `usage-check` run with
`--source disabled`, which makes `enforce-lifecycle` a deliberate no-op —
the capability stays fully wired (not deleted) and activates correctly the
day a real production Prometheus is configured and this URL is set.

### 13.3 Rollout & Migration Safety

`golang-migrate`-style pre-deploy migration via `pgcommon/pkg/migrate`, using the `realm_provisioner_migrator` role. Graceful shutdown, in order: drain HTTP and metrics servers, in-flight SQS messages and detached RP-3 conversions in parallel (30s; at-least-once delivery + `processed_events` makes redelivery safe, and an unfinished conversion's key is reclaimed after `IDEMPOTENCY_IN_PROGRESS_LEASE`), then let the outbox runner finish its current batch (`OUTBOX_DRAIN_TIMEOUT`), then close both pools on a fresh 10s budget. AWS access via IRSA (no static keys); OpenBao access via Kubernetes auth using the *same* ServiceAccount token — no AWS credentials are involved in secret access. Role/grant statements in the migration are wrapped in `DO $$ ... EXCEPTION WHEN insufficient_privilege ... END$$` blocks so the migration runs safely in dev (migrator user typically lacks `CREATEROLE`) and in production (Terraform owns `CREATE ROLE`) alike.

## 14. Testing Strategy

**Build-tag structure.** Four test suites live under `test/`, each gated by its own Go build tag so a plain `go build ./...`/`go vet ./...`/`go test ./...` compiles **none** of them: `test/unit/` (no tag — mocked, no Docker, `make test-unit`), `test/postgres/` and `test/integration/` (both `-tags=integration` — testcontainers-go, Postgres/RLS and Keycloak/OpenBao respectively, `make test-postgres`/`make test-integration`), `test/e2e/` (`-tags=e2e`, `make test-e2e`), `test/load/` (`-tags=load`, fake Keycloak, no Docker, `make test-load`). `make ci`/`make test-ci` *does* compile and run the `integration`-tagged suites for real (via `go test -tags=integration`, which must compile the package before it can even attempt to start a container) — so this is not a CI gap. It **is** a local-verification gap: a quick sanity check with a bare `go vet ./...` after changing a shared port interface (e.g. `port.KeycloakAdmin`) can look completely clean while a fake/mock implementing that interface under `-tags=integration` (or `e2e`/`load`) is left broken, undetected until the next `make ci` run or an explicit tagged vet. Run `go vet -tags=integration,e2e,load ./...` (fast, no Docker needed — vet only compiles, it doesn't execute) as a matter of habit whenever a `core/port` interface's method set changes.
- **Unit** — provisioning-flow branches (trial/direct/convert/revert); Keycloak-effect-first ordering (local commit only after the Keycloak step succeeds; compensating teardown on partial failure); idempotency-ledger replay (same key → stored result, **no** second Keycloak call); the reconcile-based MFA-role handler (converges under redelivery/reordering); `TenantReactivated` handler no-ops on a non-suspended tenant; the §11.6 validator (each CRITICAL blocks `400`, each WARN allows only with a recorded acknowledgement); realm-config apply→reconcile-on-Keycloak-failure; shard-placement tie-break determinism. `KeycloakAdmin` is mocked — assert **exact** `gocloak` call shapes (realm clone, `sub`-preserving user create, TOTP copy, WebAuthn required-action flag, forced logout).
- **Integration (testcontainers-go)** — a Postgres testcontainer with RLS (repo CRUD, the full RLS matrix, `processed_events` dedup, outbox enqueue/publish/prune, `realm_config_reconciliations` retry) **and** a Keycloak testcontainer for the `keycloak` adapter: real realm clone, user create with `sub` preservation, TOTP copy, IdP create with attribute mappers, `requires-mfa` role assign/revoke, realm disable/enable, realm export. Also asserts admin-token caching + single-flight refresh against a real token endpoint, and the rate limiter under a simulated migration burst.
- **Contract** — an `OrgMembershipClient` mock (I-8 shape) plus a test that the trial-expiry sweep touches **only** the local `tenant_realms.trial_ends_at`, never calls O&M; the `TrialStarted` consumer upserts `trial_ends_at`; all 9 published-event payloads validated against `internal/eventschema/*.json` **and** `api/asyncapi.yaml`; consumed-event decodes against producers' schemas; a test that the produced `TenantReactivated` (RP-10) and the consumed `TenantReactivated` (from Billing, `billing-realm-q`) are distinguished by `source` and never self-consumed; RP-13's report shape matches the LLD verbatim.
- **E2E** — trial provision → conversion (users migrated, TOTP copied, WebAuthn flagged, shared sessions force-logged-out) → `TenantConverted` → O&M flips realm/strategy/status; IdP configure with a region-pinned URL blocked `400`, region-neutral succeeds; an approver-level grant on `membership-realm-q` assigns `requires-mfa`; billing-lapse crons → `TenantSuspended` (realm disabled) → past retention → realm exported to S3 → `TenantOffboarded` (terminal); `TenantReactivated` on a suspended tenant re-enables the realm, on a cancelled tenant no-ops.
- **RLS (canonical, must implement all 5 cases, `test/postgres/rls_test.go`)** — Case 1: missing GUC → 0 rows. Case 2: cross-tenant `INSERT`/`UPDATE` rejected by `WITH CHECK`. Case 3: cross-tenant `SELECT` returns 0 rows. Case 4: cross-tenant `UPDATE` affects 0 rows. **Case 5 (critical)** — no GUC leakage across a pooled PgBouncer connection: pin the pool to one backend (`MaxConns=1`), tenant A writes under `SET LOCAL`, tenant B reads on the **same backend** and sees only B's rows (and 0 rows with no GUC set at all — fail-closed). Applies to all 5 tenant-scoped tables plus reconciler/consumer write paths. Plus two structural tests: every tenant-scoped table has RLS+FORCE enabled, and the three operational tables (`outbox_events`/`processed_events`/`exporter_watermarks`) deliberately do not.
- **Load** (`test/load/`) — a migration load test: convert a synthetic tenant with N users (e.g. 500), assert the client-side rate limiter keeps the Keycloak Admin API call rate in budget, conversion time scales linearly; reports p50/p95/p99 per-user migration latency.

**Coverage (rev 1.21).** Merged `make test-ci` coverage with every suite passing: **97.2%**. The drop from rev 1.17's 97.8% is new code (rev 1.20–1.21 library incorporation, AL-5 audit delivery, production-readiness fixes), not lost tests. Rev 1.21 added tests for the detached RP-3 panic/drain-deadline paths, `request_timeout`/`tenant_mismatch` mapping, producer trace links, the Pushgateway push, `cmd/migrate` against real Postgres (`cmd/migrate/main_integration_test.go`, now in `TEST_POSTGRES_PKGS`), the RLS-violation counter exporter, the inbox enqueueing a follow-on event in its claim transaction, ledger reclaim on a canceled context, and retention. One found a real bug: `pending_audit_entries.last_error` truncation could produce invalid UTF-8, so Postgres rejected `MarkFailed`/`MarkRetry`. Per package: `core/service`, `core/domain`, HTTP, consumers and most outbound adapters 100%; Keycloak adapter and `cmd/reconciler/jobs` 99.6%; `postgres` 94.7%; the composition roots 82–88%. The rest is startup `panic()` branches and unreachable `rows.Err()` paths.

**Testcontainer note.** The `test/integration/` Keycloak tests share **one** Keycloak 26 container per package run, started lazily on first use and terminated by `TestMain` (`keycloak_test.go`). Keycloak's `start-dev` spends ~60s on Quarkus augmentation on a CI runner; when each test booted its own container, starts under `make test-ci` load exceeded the wait and the package hit `go test`'s 10-minute timeout. Every test works in its own uuid-named realm, so sharing is isolation-safe. The start wait is 300s. `make test-integration` now takes ~1 minute instead of exceeding 10.

**Coverage (rev 1.17, historical).** Merged `make test-ci` coverage: **97.8%**, up from 92.2%. Rev 1.16 judged the composition-root bodies (`main()` in both binaries, `cmd/reconciler/main.go`'s `run()`, `runBusinessExporters`) unreachable without a live Postgres/Keycloak/AWS connection and set a practical ceiling of ~92.6%; that turned out to be wrong. New `cmd/server/main_integration_test.go`, `cmd/reconciler/main_integration_test.go`, and `cmd/server/exporters_integration_test.go` (`//go:build integration`, real Postgres testcontainers) close most of that gap by exploiting `signal.Notify`'s pre-empt-default-terminate behavior: sending a real `SIGINT` to the test process's own PID (`os.FindProcess(os.Getpid()).Signal(os.Interrupt)`) drives the same graceful-shutdown path production uses, safely and in-process — `main()` runs in a goroutine, polled via `/healthz`/`/readyz`/`/metrics` with `require.Eventually`, then signaled and asserted to return within 30s. `internal/core/service`, `internal/adapter/inbound/http`, and `internal/adapter/outbound/tokenservice` are all at 100%; `internal/adapter/outbound/keycloak` ~99.6%; `internal/adapter/outbound/postgres` ~98.5%. The residual ~2% is the same categories rev 1.15/1.16 already characterized as not worth chasing further: a `rows.Err()`/mid-scan-error branch repeated across several `postgres` `List*`/`groupedCounts`-backed methods, not cheaply fault-injectable against a healthy testcontainer; a couple of defensive nil-check/marshal-error branches verified genuinely unreachable against the vendored `gocloak`/`resty` source; and the thin slice of `main()`/`run()` genuinely reachable only via a live Keycloak/Token Service/AWS connection (`test/e2e`, out of scope for this measurement).

**Coverage gate ratcheted 90% → 95% (rev 1.17).** `.github/scripts/coverage-gate.sh`'s own default and `validate-test.yml`'s `COVERAGE_THRESHOLD` both moved to `'95'`, a buffer below the measured 97.8% for testcontainer-run variance — never set at or above the measured ceiling, per this repo's standing ratchet policy.

**Three new architectural-invariant CI guards (rev 1.17).** `.github/scripts/arch-lint.sh` (`make arch-lint`) gained three greps beyond RP-INV-1/RLS-6, closing gaps where this repo's own documented guarantees had no CI enforcement: an **observability invariant** (no `zap`/`logrus`/stdlib `log` import, no `prometheus.MustRegister`/`DefaultRegisterer` outside `gincommon.MetricsRegisterer()`, no independent `otel.SetTracerProvider` call); a **database invariant** (no `database/sql`/`lib/pq`, no `pgxpool.New`/`pgx.Connect` outside `pgcommon.NewPool`, no direct `golang-migrate` import); and an **events invariant** (no raw `aws-sdk-go-v2/service/sns` import anywhere, no raw `.../sqs` import outside `cmd/server/main.go`'s consumer wiring, no literal `INSERT INTO outbox_events` outside `platform-events`' own `outbox.Enqueue`, no competing local `Envelope` type — `processed_events` dedup is this service's own by design and deliberately exempt).

**Real bug found and fixed during the same pass: `TEST_INTERNAL_PKGS` vs `COVER_PKG_LIST` drift.** The Makefile has two independently-maintained package lists — `TEST_INTERNAL_PKGS` (what `make test-ci`/`_test-unit` actually invokes `go test` against) and `COVER_PKG_LIST` (derived via `go list ./cmd/... ./internal/... ./pkg/...`, what `-coverpkg` instruments for coverage). A new `internal/adapter/outbound/<name>` directory is picked up by the latter automatically but not the former — its own `_test.go` file keeps passing when run directly (`go test ./internal/adapter/outbound/<name>/...`), but silently stops contributing to `make test-ci`'s merged number. This happened to `internal/adapter/outbound/tokenservice/` (and `httpx/`) — both showed 0% in the merged report despite complete, already-passing test files, until added to `TEST_INTERNAL_PKGS`. **Check `TEST_INTERNAL_PKGS` whenever a new adapter package is added** — nothing fails loudly when this drifts, the coverage number just quietly undercounts.

**Two more real test-harness gaps found in the same pass:** `internal/adapter/inbound/consumer`'s package had no `TestMain` calling `metrics.Register(...)` (every sibling package that touches `metrics.*` package-level counters has one) — every counter these consumers touch was silently `nil` in that package's test binary, masking two dedup-helper branches (`skipDuplicate`/`ackUnknown`) that could never have incremented a real counter under test. And `test/postgres/syspool_helper_test.go`'s `realm_provisioner_migrator` GRANT list was missing `outbox_events`, needed by `OutboxPrune`'s real `outbox.Runner` against the same `sysPool` production wiring uses (mirrors the pre-existing `processed_events`/`rls_violation_log` grants already there for the same reason — the app-role migration only grants `outbox_events` to `realm_provisioner_app`, the `migrator` role's own grant being Terraform-owned in production, §20.5's pattern).

## 15. GDPR / Data Lifecycle

**PII.** The `provisioner` database holds minimal PII: `keycloak_idp_configs.config` may carry organizational (not personal) URLs/entity IDs; `provisioning_operations.result` transiently mirrors a provisioning response (can include `admin_email`). Real PII lives in Keycloak, never mirrored here. No personal fields on `tenant_realms` itself.

**Erasure.**
- **Trial expiry PII scrub** — at the 30-day-grace hard-delete (RP-C2): hard-delete the trial user from the shared `trial` realm via the Admin API (removes email/name/profile), call `NewGDPRScrubFn` to atomically delete all tenant rows from `keycloak_idp_configs`, `keycloak_idp_config_validations`, `realm_config_reconciliations`, and `provisioning_operations` (never `pending_audit_entries`: undelivered audit records, LLD v1.41) (BYPASSRLS sysPool transaction), then soft-delete the local `tenant_realms` row (retain only `tenant_id` for audit FK). The immutable Keycloak `sub` is preserved for audit continuity.
- **Tenant offboarding** (RP-C3) — the dedicated realm is exported to S3, KMS-encrypted, for compliance retention, **then** hard-deleted from Keycloak (removes all users/credentials); `NewGDPRScrubFn` atomically deletes the same four tenant-scoped PII tables (not `pending_audit_entries`, LLD v1.41) — the service **hard-fails here if `GDPRScrubFn` is nil** and never emits `TenantOffboarded` with PII intact. `TenantOffboarded` carries `export_s3_key`. The realm delete **never** precedes a confirmed export (RP-EVT-4). `offboarded` is a terminal state (RP-INV-5).

**Residency.** Single-region (`ap-south-1`) — RDS, ElastiCache, and S3 all live there. The §11.6 gate exists specifically to keep a customer-side IdP configuration from pinning the platform to a region, ahead of any future cross-region migration.

**Retention.** `tenant_realms` live rows persist for the tenant's life; soft-deleted rows are retained for the audit FK, pruned per the platform schedule. `keycloak_idp_config_validations` is append-only. S3 exports follow the compliance retention schedule. `outbox_events`/`processed_events` are pruned on their own operational retention timers; `provisioning_operations` (whose `result` can carry `admin_email`) after 30 days and `rls_violation_log` (which carries `query_text`) after 90, by `ledger-prune` (see §13.1 above).

## 18. Integration Points

### 18.1 `org-membership`

- O&M → RP (`RealmProvisionerClient`, byte-exact signatures required): `CreateInvitedUser` → RP-5; `DeleteUser` → RP-15 (idempotent); `PatchRealmConfig` → RP-8 (local-intent-first); `RevokeUserSessions` → RP-16 (best-effort/fail-open from O&M's perspective); `ResetMFA` → RP-9 (fail-closed).
- RP → O&M: `OrgMembershipClient.HasResidualMFARequirement` — backs the `membership-realm-q` MFA-revoke decision (§ above); read-only, no write path from RP into O&M's own tables.
- O&M → events → RP: `TrialStarted` on `tenant-realm-q` — idempotent upsert of `trial_ends_at` only; RP never queries O&M for trial timing, it's push-only.
- RP → events → O&M: all 9 produced events on `iam-tenant-events`, consumed by O&M's own `tenant-orgm-q`.

### 18.2 `billing-service`

- Billing → RP (mesh calls, not events): RP-3 (`convert`), RP-4 (`revert-conversion`), RP-10 (`reactivate`, non-operator source).
- Billing → events → RP: `TenantReactivated` on `billing-realm-q` (same event *name* as RP's own produced `TenantReactivated`, but a different topic/producer — no self-consumption; RP-D13 distinguishes them by `source`).
- RP → O&M (not Billing directly, **open item** below): the subscription-lapse sweep (RP-C3) is specified to read subscription status from O&M, since RP stores no subscription/plan data of its own.

### 18.3 `signup-bff` / operator tooling

- Signup BFF → RP: RP-1 (`provision-trial`), RP-2 (`provision-direct`).
- Operator tooling → RP: RP-13 (`idp-region-portability` audit), RP-14 (`suspend`).

### 18.4 `iam-token-service` (§2.5, RP-17/RP-18)

- RP → Token Service: **TS-4** `RegisterPrincipal` and **TS-1** `IssueOrRotateCredential`, called from the shared `mintPlatformAutomation` helper on every RP-1/RP-2/RP-3 mint and RP-4 revert-remint. **TS-3** `ReadPrincipal` exists on `port.TokenServiceClient` (RP-18 — RP is the caller here, not a new RP-hosted endpoint) but has no MVP call site yet.
- RP → Token Service (no network call): `JWKSURLFor(tenant_id)` is pure string construction against `TOKEN_SERVICE_BASE_URL`, used to mint the Keycloak client's `jwks.url` attribute.
- Keycloak → Token Service (not RP, and not a call this service brokers): Keycloak's own outbound `client-jwt` authenticator fetches the JWKS directly from Token Service's deliberately-unauthenticated route (§10.6). RP has no visibility into or control over that fetch.
- Token Service's own scheduled-rotation path (`cmd/scheduler`) can rotate a credential by calling its own TS-1 directly, bypassing RP entirely — the new key then needs *something* to call this service's RP-17 afterward for Keycloak to recognize it. Wiring that call automatically is an open follow-up, not yet done (§2.5's own documented gap).

## Production go-live checklist

Things the code cannot do for itself. Each is a one-time platform/ops step; skipping one leaves a feature dark or a release failing, not a silent data problem.

| # | Step | Without it |
|---|---|---|
| 1 | **Terraform grants for `realm_provisioner_migrator`** (the sysPool / reconciler role): `SELECT, UPDATE, DELETE` on `pending_audit_entries`; `SELECT, DELETE` on `rls_violation_log`; `SELECT, INSERT, UPDATE` on `exporter_watermarks`; `DELETE` on `provisioning_operations` (plus the existing reconciler grants) | AL-5 audit drainer, RLS-6 alerts and `ledger-prune` fail with permission errors |
| 2 | **`existingSecret` holds every `envFromSecret` key**: the three DSNs, `KEYCLOAK_ADMIN_CLIENT_SECRET`, `SNS_TOPIC_TENANT_ARN`, `SQS_{MEMBERSHIP,TENANT,BILLING}_REALM_QUEUE_URL`, `DOCS_AUTH_TOKEN` | `helm upgrade` fails to render |
| 3 | **IRSA**: `serviceAccount.annotations` → `eks.amazonaws.com/role-arn` with `deploy/iam/policy.json` (SNS, SQS incl. the `-dlq` queues' `SendMessage`/`GetQueueAttributes`, Glue, S3/KMS) | Outbox publish, consumers, DLQ forwarding and realm export fail |
| 4 | **Inbound queues have a RedrivePolicy** (`maxReceiveCount=5` → `-dlq`) | Startup panics (`DLQ preflight`) |
| 5 | **Glue registry `iam-tenant-events`** has this build's 9 schema definitions registered and `AVAILABLE` (`make schema-verify`) | Startup panics resolving a schema |
| 6 | **OpenBao**: Kubernetes-auth role `iam-realm-provisioner` bound to this ServiceAccount; Keycloak admin credential at `BAO_KEYCLOAK_ADMIN_PATH` | No IdP configuration, no Keycloak admin token |
| 7 | **`PROMETHEUS_PUSHGATEWAY_URL`** set, and the Pushgateway scrape uses `honor_labels: true`; `networkPolicy.pushgatewayPort` matches it | Reconciler metrics and the reconciler/SLO-3 alerts are dark |
| 8 | **`networkPolicy.databasePorts`** includes the PgBouncer and direct-Postgres ports in use | Pods and the migrate Job cannot reach the database |
| 9 | **`AUDIT_LOG_BASE_URL`** once iam-audit-log (rev 0.31+, for `legal.terms.accepted`) is deployed | `TenantIdpConfigChanged` and `TermsAccepted` entries accumulate in `pending_audit_entries` (`IAMRealmProvisionerAuditEntriesUndelivered`) |
| 10 | **Trial tenant size vs `CONVERT_TIMEOUT`** (10m ≈ 2,000 users at the LLD's 0.3s/user) | Larger conversions are cut off and retried |
| 11 | **`DEPLOY_GATE_ENABLED`** + `KUBECONFIG_B64` for the release workflow's Helm deploy and health gate | Releases publish images but never deploy/verify |
| 12 | **Caller migration to RFC 9457 Problem Details** before deploying this service for the first time. All callers (Org & Membership, Billing, Signup BFF) that parse this service's error responses must be updated **before** this service is deployed. The wire format changed from the flat shape `{"error": "tenant_realm_not_found", "status": 404, "trace_id": "..."}` to RFC 9457 `{"type": "...", "title": "...", "status": 404, "errorCode": "TENANT_REALM_NOT_FOUND", "traceId": "...", "instance": "..."}`. Specifically: (a) branch on `errorCode` (UPPER_SNAKE_CASE), not `error` (lower_snake_case); (b) struct binding failures now return `VALIDATION_FAILED` (with `validationErrors[]`) instead of `INVALID_REQUEST` — callers that branch on `INVALID_REQUEST` for missing-field errors must also check `VALIDATION_FAILED`; (c) use `platform-gincommon/v2/pkg/problem`'s `FromResponse()` to parse responses, or decode `{"errorCode": string, "status": int, "traceId": string}`. This is a breaking wire contract change. Coordinate as a simultaneous multi-service deploy, or deploy callers first (they can tolerate the new format before this service is live). | Every caller receives an unparseable error response; error-branch logic silently falls through to the wrong code path |

## 20. Open Items — Operational Impact

- **RP-C3's suspend phase degrades to a no-op WARN if `ORG_MEMBERSHIP_BASE_URL` is unset.** This is a deliberate, not a missing, degradation: `cmd/reconciler/main.go` wires `port.SubscriptionLapseReader` (`internal/adapter/outbound/orgmembership`, O&M's I-16 `GET /api/v1/internal/subscription-lapses`) only when that env var is set, so an environment without it (e.g. a dev/CI run with no O&M instance) doesn't fail the whole job — the offboard phase (retention-driven, purely local) still runs. Set `ORG_MEMBERSHIP_BASE_URL` in every environment where automatic billing-lapse suspension should actually happen.
- **OpenBao Kubernetes auth is untested outside a real cluster.** The Kubernetes ServiceAccount JWT the auth flow depends on (`/v1/auth/kubernetes/login`) only exists inside a real cluster with the correct service account bound (`deploy/openbao/role.tf.example`). In local-dev/CI without that binding, RP-6 (configure IdP) and RP-7 (delete IdP) return `500` on the OpenBao write/delete step — expected there, not a production defect. Verify OpenBao connectivity as part of any staging/production rollout checklist before relying on IdP configuration endpoints.
- **RP-9's route parameter** is internally named `:keycloak_user_id` rather than the frozen §25 spelling `:uid` — Gin requires one wildcard name per path position across sibling routes sharing a prefix, and RP-15/RP-16 already use `:keycloak_user_id` at that position. Routing-internals detail only; the actual URL a caller hits is unchanged.
- **RP-13's `failing_tenants` field** serializes as JSON `null` rather than `[]` when empty — a cosmetic gap, not a correctness issue; treat `null` and `[]` as equivalent ("no failing tenants") when reading the report.
- **A fresh platform-automation mint can leave Token Service's principal record pointing at a Keycloak client this service then deletes (§2.5).** If TS-4's `RegisterPrincipal` succeeds but TS-1's `IssueOrRotateCredential` then fails on that same mint attempt, `mintPlatformAutomation` best-effort deletes the just-minted client — but there is no Token Service API to revert the TS-4 carry-over, so Token Service is left believing the principal's `keycloak_client_id` points at a client that no longer exists. This self-heals automatically on the next successful retry of the same Idempotency-Key (retrying re-runs the full mint sequence and re-points the principal at whatever client that retry produces) — if an operator suspects this state (e.g. a tenant's automation calls are failing right after a provisioning/conversion/revert attempt that itself returned an error), the fix is simply to have the caller retry the original request with the same `Idempotency-Key` header, not to intervene manually in Keycloak or Token Service.
- **`rls_violations_total`'s exporter needs a Terraform-side grant this repo cannot provide.** `GaugeRepository.RLSViolationCounts` (backing the two RLS-6 alerts, `IAMRealmProvisionerRLSCrossTenantAccess`/`RLSMissingGUC`) reads `rls_violation_log` and reads/writes its own persisted high-water mark in `exporter_watermarks` (§4.2), both via `sysPool`, i.e. as `realm_provisioner_migrator`. Every privilege that role holds is provisioned by Terraform outside this repo (§10.1) — the domain migration grants it nothing on either table. Until that Terraform is updated to `GRANT SELECT ON rls_violation_log` and `GRANT SELECT, INSERT, UPDATE ON exporter_watermarks TO realm_provisioner_migrator`, this exporter fails every tick with a permission-denied error in any real environment (logged at WARN, not fatal — the previous counter value is retained, same as any other exporter query failure). `test/postgres/syspool_helper_test.go` grants both directly since the test harness doesn't run the real Terraform.

Full operator procedures for every headline scenario (Keycloak degraded, realm-config not landing, offboarding export failed, late conversion reversal, stuck idempotency key, pre-migration IdP audit, outbox/SQS/DLQ health, reconciler job health, emergency operator suspension) are in `docs/runbook.md`.
