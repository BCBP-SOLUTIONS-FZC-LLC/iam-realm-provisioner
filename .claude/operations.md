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

RP makes **no** end-user authorization decision — that is AuthZ Enrichment's job. Callers are trusted mesh peers (internal-route guard + mTLS); the `x-tenant-id` header scopes the request and RLS enforces isolation. RP-13/RP-14 additionally require an operator role in `x-tenant-roles`/the platform-admin claim, re-checked at the handler (defense-in-depth, network isolation is primary).

### 10.6 §11.6 Region-Agnostic IdP Configuration Checklist

Checks every SAML/OIDC config field against a fixed list of AWS region substrings and against the one platform-wide region-neutral auth host (`auth.bcbpsolutions.com`).

- **CRITICAL (block, `400 idp_region_portability_failed`), 12 total:** no config value contains an HTML/XML injection sequence; SAML SSO/SLO URL hostnames carry no AWS-region substring; a customer-provided signing cert is present; ACS/SLO/EntityID point at the region-neutral host; OIDC issuer/discovery is region-neutral; OIDC redirect URIs are region-neutral; OIDC audience matches the region-neutral client ID; an OIDC `client_id` is present; an OIDC `issuer` is a non-empty, valid HTTPS URL.
- **WARN (allow with a recorded acknowledgement, else `422 idp_warnings_unacknowledged`):** SAML `NameIDFormat != "transient"`; custom attribute-mapper URNs are region-free; SAML signature algorithm ≥ `RSA-SHA256`; OIDC PKCE `S256`. Acknowledgement recorded with the admin's `user_id` + timestamp in `keycloak_idp_config_validations.acknowledgements`.
- RP-13's whole-estate audit re-runs the same `ValidateIdpConfig` function against every stored config's non-secret `Config` (never re-reading secrets from OpenBao) to compute `{total_tenants, total_idp_configs, passing, warnings_only, failing, failing_tenants[]}`.

## 11. Observability

### 11.1 Metrics

All `iam_realm_provisioner_`-prefixed (frozen §25 name inventory), registered in `internal/adapter/outbound/metrics/metrics.go`:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `iam_realm_provisioner_provision_total` | Counter | `kind` (`trial`\|`direct`\|`convert`), `result` | Provisioning attempts |
| `iam_realm_provisioner_users_migrated_total` | Counter | — | Users migrated across a conversion |
| `iam_realm_provisioner_realm_active_gauge` | Gauge | `strategy` | Active realms by strategy |
| `iam_realm_provisioner_keycloak_call_duration_seconds` | Histogram | `op` | Keycloak Admin API call latency |
| `iam_realm_provisioner_keycloak_call_failures_total` | Counter | `op` | Keycloak Admin API call failures |
| `iam_realm_provisioner_admin_token_refresh_total` | Counter | `trigger` (`proactive`\|`401`) | Admin token cache refresh — a `401`-triggered spike signals credential rotation, not an outage |
| `iam_realm_provisioner_idp_validation_total` | Counter | `outcome` | §11.6 validation run outcomes |
| `iam_realm_provisioner_mfa_role_sync_total` | Counter | `action` (`assign`\|`revoke`) | `membership-realm-q` MFA role reconciles |
| `iam_realm_provisioner_realm_config_pending_gauge` | Gauge | — | `count(*) WHERE status='pending'` in `realm_config_reconciliations` |
| `iam_realm_provisioner_trial_expiry_processed_total` | Counter | — | RP-C1 rows processed |
| `iam_realm_provisioner_trial_expiry_deferred_total` | Counter | — | RP-C1 rows deferred — **must increment on every defer**, not just be registered at startup, or the "sustained >0" alert can never fire |
| `iam_realm_provisioner_lapse_processed_total` | Counter | `transition` (`suspend`\|`offboard`) | RP-C3 transitions |
| `iam_realm_provisioner_realm_export_total` | Counter | `result` | Offboarding export attempts (RP-C3) |
| `iam_realm_provisioner_user_delete_total` | Counter | `result` | RP-15 |
| `iam_realm_provisioner_session_revoke_total` | Counter | `result` | RP-16 |
| `iam_realm_provisioner_mfa_reset_total` | Counter | `result` | RP-9 (fail-closed semantics) |
| `iam_realm_provisioner_template_drift` | Gauge/Counter | `realm_version` | Report-only realm-template drift (RP-3) |

Plus passthrough `http_*` (from `platform-gincommon`), `events_*`/`outbox_*`/`sqs_*` (from `platform-events`, including `outbox_dead_letters_total`), and `pgcommon_*` pool/query/retry instruments (from `platform-pgcommon`'s `pgmetrics.InitWithRegisterer`). All three land on the registry `gincommon.MetricsRegisterer()` returns — `cmd/server/main.go` primes `ObservabilityMiddlewares` first (gincommon's metrics-init API), then registers business / events / pg collectors onto it, rather than assuming `DefaultRegisterer`. No projection-lag metric — this service is a producer, not a projector.

> **DB-state gauges.** `realm_active_gauge`, `realm_config_pending_gauge`, and `template_drift` are refreshed every 5 minutes by exporter goroutines in `cmd/server` (`runBusinessExporters` → `pgadapter.GaugeRepository`, querying the BYPASSRLS sysPool), emitting once at startup so they are populated before the first scrape. A failing query logs `gauge exporter query failed` at WARN and keeps the previous value, so **a flat gauge plus that log line means the exporter is broken, not that the count is stable** — check the log before trusting a flat line. `template_drift` series disappear when a version stops drifting (the vec is reset each poll); `realm_active_gauge` reports an explicit `0` instead.
>
> **Not-yet-armed instruments.** `trial_expiry_*`, `lapse_processed_total`, and `realm_export_total` are incremented correctly but only inside the reconciler CronJob, which exits before a scrape lands and exposes no `/metrics` listener; exporting them needs a Pushgateway or DB-backed state the server reads (the sibling `iam-user-profile` reconciler has the same limitation). Treat silence on these as "no signal", not "no problem". See LLD §11.2.

**Alerts (`deploy/monitoring/app-alerts.yml`):**
- `IAMRealmProvisionerDown` — no healthy instances for 2m → **critical** (blocks all 16 endpoints + all 3 SQS consumers; the reconciler CronJobs run as separate pods and are not covered)
- `IAMRealmProvisionerReplicasMissing` — fewer than 2 healthy replicas for 5m → warning
- `IAMRealmProvisionerHighErrorRate` — 5xx rate > 5% for 5m → warning
- `IAMRealmProvisionerCriticalErrorRate` — 5xx rate > 20% for 2m → **critical** — escalate immediately, RP-INV-1's single Keycloak writer means every tenant realm operation funnels through this one service
- `keycloak_call_failures_total` rate > 5% for 5m → warning
- `trial_expiry_deferred_total > 0` sustained 1h → warning
- `realm_config_pending_gauge > 0` sustained 30m → warning (RP-INV-6 risk — stored intent and actual Keycloak state have diverged)
- `realm_export_total{result="fail"} > 0` → **critical** (compliance-blocking)
- `outbox_dead_letters_total` increase > 0 over 30m → warning
- `{membership,billing,tenant}-realm-q` DLQ depth > 0 → warning
- `admin_token_refresh_total{trigger="401"}` spike → possible credential-rotation issue, not necessarily an outage

### 11.2 Tracing

`gincommon.InitTracingFromEnv()` in both binaries (always — in-process spans get valid trace IDs even without a collector; OTLP export is a no-op until `OTEL_EXPORTER_OTLP_ENDPOINT` is set), plus `pgcommon.Config.Tracer` for a `db.query` span per query. W3C `traceparent` is injected on every outbound HTTP call (O&M, OpenBao, Keycloak) by the shared `adapter/outbound/httpx` transport; SNS instead carries `trace_id` on the outbox envelope, so provision → event → O&M-projection is one trace. Keycloak spans are named `keycloak.<op>` (e.g. `keycloak.create_realm`, `keycloak.migrate_user`); a CronJob run is rooted at `reconciler.<job>`.

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
| `SYSTEM_DATABASE_URL` | (required) | `realm_provisioner_migrator` (`BYPASSRLS`) DSN — the reconciler's cross-tenant sweeps use this, never the app role. Required even in dev. |
| `MIGRATION_DATABASE_URL` | (required) | Direct (non-PgBouncer) DSN — only `realm_provisioner_migrator` can run `outbox.ApplySchema` / the domain migration; `realm_provisioner_app` has no `CREATE` on schema `public` |
| `AWS_REGION`, `AWS_ENDPOINT_URL` | `ap-south-1` / — | Region + LocalStack endpoint override for local dev |
| `SNS_TOPIC_TENANT_ARN` | (required in prod) | Single outbound topic, `iam-tenant-events` |
| `GLUE_REGISTRY_TENANT_NAME` | (required in prod) | Unset → `NoopCodec` (plain JSON, local dev) |
| `SQS_MEMBERSHIP_REALM_QUEUE_URL` / `SQS_TENANT_REALM_QUEUE_URL` / `SQS_BILLING_REALM_QUEUE_URL` | (required in prod) | The 3 inbound queues |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_DRAIN_TIMEOUT` / `OUTBOX_RETENTION_DAYS` | `500ms` / `50` / `5` / `30s` / `8` | Outbox runner tunables |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | Consumer dedup retention (must exceed the 7-day SQS message lifetime) |
| `KEYCLOAK_BASE_URL` | (required) | e.g. `https://auth.bcbpsolutions.com` — fail-fast on the dev default outside `APP_ENV=dev` |
| `KEYCLOAK_ADMIN_REALM` | `master` | |
| `KEYCLOAK_ADMIN_CLIENT_ID` / `KEYCLOAK_ADMIN_CLIENT_SECRET` | `iam-realm-provisioner` / (from OpenBao via ESO) | Fail-fast on empty/dev-default outside `APP_ENV=dev` |
| `KEYCLOAK_RATE_LIMIT_RPS` | `20` | Client-side rate limiter — absorbs bursts during mass migration |
| `KEYCLOAK_TIMEOUT_MS` | `8000` (as `8s` in `.env-example`) | Per-operation timeout budget |
| `BAO_ADDR` | e.g. `https://openbao.iam.svc.cluster.local:8200` | OpenBao — Kubernetes auth (pod ServiceAccount token) |
| `BAO_AUTH_ROLE` / `BAO_KV_MOUNT` / `BAO_IDP_PATH_PREFIX` / `BAO_KEYCLOAK_ADMIN_PATH` | `iam-realm-provisioner` / `secret` / `iam/realm-provisioner/idp` / `iam/realm-provisioner/keycloak-admin` | |
| `SHARED_TRIAL_REALM` / `REALM_TEMPLATE_VERSION` | `trial` / `v1` | Shared trial realm name + active dedicated-realm template version |
| `ORG_MEMBERSHIP_BASE_URL` | (required) | O&M client (I-8 residual-MFA check) — fail-fast on the dev default outside `APP_ENV=dev` |
| `REALM_EXPORT_BUCKET` / `REALM_EXPORT_KMS_KEY` | `iam-realm-exports` / `alias/iam-realm-export` | Offboarding export target |
| `TRIAL_GRACE_DAYS` / `CANCELLED_GRACE_DAYS` / `RETENTION_DAYS` | `30` / `30` / `90` | Lifecycle timers |
| `RECONCILER_BATCH_LIMIT` / `RECONCILER_TIMEOUT` / `RECONCILER_MAX_ATTEMPTS` | `100` / `5m` / `20` | Reconciler-job tunables — `RECONCILER_MAX_ATTEMPTS` is RP-C4's poison-pill cap (§13.1) |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | `true` / — | AsyncAPI viewer gating — mounted always outside `APP_ENV=production`; opt-in via `DOCS_ENABLED` in prod, optionally bearer-gated |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | OTLP collector — provider always installed; export no-op until set |

**Fail-fast:** startup panics if `KEYCLOAK_BASE_URL` / `KEYCLOAK_ADMIN_CLIENT_SECRET` / `ORG_MEMBERSHIP_BASE_URL` are empty or hold a dev-default value outside `APP_ENV=dev`.

**Realm template versioning:** the in-repo versioned `deploy/keycloak/realm-template.json` (IAM-Platform-owned, PR-reviewed) is embedded/mounted at deploy time, not fetched from S3 or another external source. `REALM_TEMPLATE_VERSION` selects the active version; promotion is CI/CD-gated (dev→staging→prod) by Keycloak import validation + realm-creation smoke tests. A version bump has **no retroactive application** — it only affects newly provisioned realms; there is no bulk re-template job. Drift detection is report-only (no auto-remediation): each realm's `tenant_realms.realm_template_version` is compared against the current `REALM_TEMPLATE_VERSION` and surfaced via `iam_realm_provisioner_template_drift{realm_version}`.

## 13. Deployment & Scaling

**Topology.** EKS namespace `iam` (`ap-south-1`); RDS `provisioner` (Multi-AZ, PgBouncer tx pooling); ElastiCache Valkey (`rp:` keyspace); SNS topic `iam-tenant-events` + 3 consumer SQS queues each with a DLQ; Glue registry `iam-tenant-events`; S3 bucket for realm exports; one Envoy upstream, mesh-internal only.

**Two workloads from one image:** the `server` Deployment (API + 3 SQS consumers + outbox runner) and the `reconciler`, run as K8s CronJobs (`cmd/reconciler --job=<name>`), share one Dockerfile — the CronJob template overrides `command` to `/iam-realm-provisioner-reconciler` with `--job=<jobName>`.

**Scaling.** `replicaCount: 2` (manual, low-volume). HPA conservative (min 2 / max 4, CPU+memory). The real bottleneck during mass migration is the Keycloak Admin API itself (not this service's CPU) — governed by the client-side rate limiter, not horizontal scaling.

### 13.1 CronJobs

All six defined in one Helm manifest (`deploy/helm/templates/cronjobs.yaml`), dispatched via `cmd/reconciler/main.go`'s `registry` map (exact `--job=` values below):

| CronJob (`--job=`) | LLD ref | Schedule | Purpose |
|---|---|---|---|
| `trial-expiry` | RP-C1 | `0 2 * * *` | Sweep `tenant_realms WHERE strategy='shared' AND status='active' AND trial_ends_at < now()` (`idx_tenant_realms_trial_exp`) — **never queries O&M**. Calls the idempotent RP-12 effect per tenant, keyed `"trial-expiry:<tenant_id>:<trial_ends_at>"` so a changed `trial_ends_at` naturally produces a fresh key. |
| `trial-hard-delete` | RP-C2 | `0 3 * * *` | For trials past `TRIAL_GRACE_DAYS` (30) beyond expiry: hard-delete users from the shared realm via the Admin API, soft-delete/scrub the local `tenant_realms` row (retain only `tenant_id` for audit-FK, preserve `sub`). Runs entirely against `SysPool` (`BYPASSRLS`) with raw SQL, not through the repository port. |
| `subscription-lapse` | RP-C3 | `0 2 * * *` | Two-phase suspend→offboard sweep. Suspend polls O&M's I-16 `GET /api/v1/internal/subscription-lapses` (skipped with a WARN if `ORG_MEMBERSHIP_BASE_URL` is unset); offboard always runs against local `suspension_source='billing_lapse'` rows past `RETENTION_DAYS`, independent of whether the suspend phase ran. |
| `realm-config-reconcile` | RP-C4 | `*/10 * * * *` | Retries every `realm_config_reconciliations` row in `status='pending'`: re-resolve the realm, re-attempt `ApplyRealmConfig`; success → `MarkApplied`, failure → `MarkFailed(lastErr, RECONCILER_MAX_ATTEMPTS)`, which leaves the row pending for the next tick unless its attempt count has just reached `RECONCILER_MAX_ATTEMPTS` (default 20 — poison-pill cap), in which case it moves atomically to the terminal `failed` status instead of retrying forever. |
| `outbox-prune` | — | `0 4 * * *` (this chart's own choice, not LLD-pinned) | Calls `platform-events`' `outbox.Runner.PrunePublished` at `OUTBOX_RETENTION_DAYS` (default 8) retention. |
| `processed-events-prune` | — | `30 4 * * *` (this chart's own choice, not LLD-pinned) | Prunes `processed_events` rows older than `PROCESSED_EVENTS_TTL_DAYS` (default 8). |

Every job is idempotent per tenant (safe to re-run) and batched (`RECONCILER_BATCH_LIMIT`, default 100). **Instrumentation requirement:** every deferring job must *increment* its `*_deferred_total` counter on every defer, not merely register it at startup — otherwise a "deferred > 0 sustained" alert can never fire even when the job is genuinely stuck.

**Helm chart** (`deploy/helm/`) mirrors sibling charts: `deployment.yaml`, `cronjobs.yaml` (all 6 jobs from one templated range), `hpa.yaml`, `pdb.yaml`, `service.yaml` (+ metrics port), `serviceaccount.yaml` (IRSA annotation), `servicemonitor.yaml`, `prometheusrule.yaml`, `networkpolicy.yaml` (monitoring-namespace scrape + mesh egress to Keycloak/O&M/OpenBao/AWS), `securitypolicy.yaml` (Envoy internal-route auth), `secret.yaml` (ESO `ExternalSecret` refs against an OpenBao `ClusterSecretStore`), `httproute.yaml`.

`deploy/iam/` — pod IAM policy for **AWS resources only** (`sns:Publish` to `iam-tenant-events`; `sqs:*` on the 3 queues + DLQs; `glue:GetSchemaVersion`; `s3:PutObject`/`GetObject` on the export bucket; `kms:*` on the export key). **Secret access is not in this IAM policy** — it is governed entirely by OpenBao.

`deploy/openbao/` — `policy.hcl` (grants on the IdP-secret path prefix + the Keycloak-admin path) + `role.tf.example` (Kubernetes-auth role binding the pod ServiceAccount).

`deploy/keycloak/realm-template.json` — the versioned dedicated-realm template (§12.2 above).

### 13.2 CI/CD (`.github/workflows/`)

- **`validate-test.yml`** — unit + integration (`go test -race`), coverage gate (`.github/scripts/coverage-gate.sh`), Postgres + Keycloak testcontainers.
- **`validate-quality.yml`** — `golangci-lint`, `.go-arch-lint.yml` check (`.github/scripts/arch-lint.sh`), the no-session-scoped-`SET`-GUC grep (RLS-6), the no-`gocloak`-outside-`adapter/keycloak` grep (RP-INV-1). It carries no schema step itself — the AsyncAPI-vs-Glue drift gate (pinned `platform-schemagov` validation of `api/asyncapi.yaml` against the `iam-tenant-events` Glue registry, failing the PR on drift) lives in `schema-registry.yml`'s `pr-check` job instead; see 13.2.1 below for the full schema-governance pipeline that same tool also drives.
- **`ci.yml`** — orchestrates the above, plus `build-image` (multi-stage Dockerfile + provenance scripts), a Trivy scan, a smoke check (`/healthz` returns 200, `/asyncapi.yaml` serves a real spec), `pr-summary`, and pushes to GHCR on the default branch.
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

`golang-migrate`-style pre-deploy migration via `pgcommon/pkg/migrate`, using the `realm_provisioner_migrator` role. Graceful shutdown: drain HTTP, let in-flight SQS messages finish (at-least-once delivery + `processed_events` makes redelivery safe), let the outbox runner finish its current batch (`OUTBOX_DRAIN_TIMEOUT`). AWS access via IRSA (no static keys); OpenBao access via Kubernetes auth using the *same* ServiceAccount token — no AWS credentials are involved in secret access. Role/grant statements in the migration are wrapped in `DO $$ ... EXCEPTION WHEN insufficient_privilege ... END$$` blocks so the migration runs safely in dev (migrator user typically lacks `CREATEROLE`) and in production (Terraform owns `CREATE ROLE`) alike.

## 14. Testing Strategy

- **Unit** — provisioning-flow branches (trial/direct/convert/revert); Keycloak-effect-first ordering (local commit only after the Keycloak step succeeds; compensating teardown on partial failure); idempotency-ledger replay (same key → stored result, **no** second Keycloak call); the reconcile-based MFA-role handler (converges under redelivery/reordering); `TenantReactivated` handler no-ops on a non-suspended tenant; the §11.6 validator (each CRITICAL blocks `400`, each WARN allows only with a recorded acknowledgement); realm-config apply→reconcile-on-Keycloak-failure; shard-placement tie-break determinism. `KeycloakAdmin` is mocked — assert **exact** `gocloak` call shapes (realm clone, `sub`-preserving user create, TOTP copy, WebAuthn required-action flag, forced logout).
- **Integration (testcontainers-go)** — a Postgres testcontainer with RLS (repo CRUD, the full RLS matrix, `processed_events` dedup, outbox enqueue/publish/prune, `realm_config_reconciliations` retry) **and** a Keycloak testcontainer for the `keycloak` adapter: real realm clone, user create with `sub` preservation, TOTP copy, IdP create with attribute mappers, `requires-mfa` role assign/revoke, realm disable/enable, realm export. Also asserts admin-token caching + single-flight refresh against a real token endpoint, and the rate limiter under a simulated migration burst.
- **Contract** — an `OrgMembershipClient` mock (I-8 shape) plus a test that the trial-expiry sweep touches **only** the local `tenant_realms.trial_ends_at`, never calls O&M; the `TrialStarted` consumer upserts `trial_ends_at`; all 9 published-event payloads validated against `internal/eventschema/*.json` **and** `api/asyncapi.yaml`; consumed-event decodes against producers' schemas; a test that the produced `TenantReactivated` (RP-10) and the consumed `TenantReactivated` (from Billing, `billing-realm-q`) are distinguished by `source` and never self-consumed; RP-13's report shape matches the LLD verbatim.
- **E2E** — trial provision → conversion (users migrated, TOTP copied, WebAuthn flagged, shared sessions force-logged-out) → `TenantConverted` → O&M flips realm/strategy/status; IdP configure with a region-pinned URL blocked `400`, region-neutral succeeds; an approver-level grant on `membership-realm-q` assigns `requires-mfa`; billing-lapse crons → `TenantSuspended` (realm disabled) → past retention → realm exported to S3 → `TenantOffboarded` (terminal); `TenantReactivated` on a suspended tenant re-enables the realm, on a cancelled tenant no-ops.
- **RLS (canonical, must implement all 5 cases, `test/postgres/rls_test.go`)** — Case 1: missing GUC → 0 rows. Case 2: cross-tenant `INSERT`/`UPDATE` rejected by `WITH CHECK`. Case 3: cross-tenant `SELECT` returns 0 rows. Case 4: cross-tenant `UPDATE` affects 0 rows. **Case 5 (critical)** — no GUC leakage across a pooled PgBouncer connection: pin the pool to one backend (`MaxConns=1`), tenant A writes under `SET LOCAL`, tenant B reads on the **same backend** and sees only B's rows (and 0 rows with no GUC set at all — fail-closed). Applies to all 5 tenant-scoped tables plus reconciler/consumer write paths. Plus two structural tests: every tenant-scoped table has RLS+FORCE enabled, and the three operational tables (`outbox_events`/`processed_events`/`exporter_watermarks`) deliberately do not.
- **Load** (`test/load/`) — a migration load test: convert a synthetic tenant with N users (e.g. 500), assert the client-side rate limiter keeps the Keycloak Admin API call rate in budget, conversion time scales linearly; reports p50/p95/p99 per-user migration latency.

## 15. GDPR / Data Lifecycle

**PII.** The `provisioner` database holds minimal PII: `keycloak_idp_configs.config` may carry organizational (not personal) URLs/entity IDs; `provisioning_operations.result` transiently mirrors a provisioning response (can include `admin_email`). Real PII lives in Keycloak, never mirrored here. No personal fields on `tenant_realms` itself.

**Erasure.**
- **Trial expiry PII scrub** — at the 30-day-grace hard-delete (RP-C2): hard-delete the trial user from the shared `trial` realm via the Admin API (removes email/name/profile), soft-delete the local row, scrub residual PII, retain only `tenant_id` for the audit FK. The immutable Keycloak `sub` is preserved for audit continuity.
- **Tenant offboarding** (RP-C3) — the dedicated realm is exported to S3, KMS-encrypted, for compliance retention, **then** hard-deleted from Keycloak (removes all users/credentials); the local row is soft-deleted/scrubbed. `TenantOffboarded` carries `export_s3_key`. The realm delete **never** precedes a confirmed export (RP-EVT-4). `offboarded` is a terminal state (RP-INV-5).

**Residency.** Single-region (`ap-south-1`) — RDS, ElastiCache, and S3 all live there. The §11.6 gate exists specifically to keep a customer-side IdP configuration from pinning the platform to a region, ahead of any future cross-region migration.

**Retention.** `tenant_realms` live rows persist for the tenant's life; soft-deleted rows are retained for the audit FK, pruned per the platform schedule. `keycloak_idp_config_validations` is append-only. S3 exports follow the compliance retention schedule. `outbox_events`/`processed_events` are pruned on their own operational retention timers (see §13.1 above).

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

## 20. Open Items — Operational Impact

- **RP-C3's suspend phase degrades to a no-op WARN if `ORG_MEMBERSHIP_BASE_URL` is unset.** This is a deliberate, not a missing, degradation: `cmd/reconciler/main.go` wires `port.SubscriptionLapseReader` (`internal/adapter/outbound/orgmembership`, O&M's I-16 `GET /api/v1/internal/subscription-lapses`) only when that env var is set, so an environment without it (e.g. a dev/CI run with no O&M instance) doesn't fail the whole job — the offboard phase (retention-driven, purely local) still runs. Set `ORG_MEMBERSHIP_BASE_URL` in every environment where automatic billing-lapse suspension should actually happen.
- **OpenBao Kubernetes auth is untested outside a real cluster.** The Kubernetes ServiceAccount JWT the auth flow depends on (`/v1/auth/kubernetes/login`) only exists inside a real cluster with the correct service account bound (`deploy/openbao/role.tf.example`). In local-dev/CI without that binding, RP-6 (configure IdP) and RP-7 (delete IdP) return `500` on the OpenBao write/delete step — expected there, not a production defect. Verify OpenBao connectivity as part of any staging/production rollout checklist before relying on IdP configuration endpoints.
- **RP-9's route parameter** is internally named `:keycloak_user_id` rather than the frozen §25 spelling `:uid` — Gin requires one wildcard name per path position across sibling routes sharing a prefix, and RP-15/RP-16 already use `:keycloak_user_id` at that position. Routing-internals detail only; the actual URL a caller hits is unchanged.
- **RP-13's `failing_tenants` field** serializes as JSON `null` rather than `[]` when empty — a cosmetic gap, not a correctness issue; treat `null` and `[]` as equivalent ("no failing tenants") when reading the report.
- **`rls_violations_total`'s exporter needs a Terraform-side grant this repo cannot provide.** `GaugeRepository.RLSViolationCounts` (backing the two RLS-6 alerts, `IAMRealmProvisionerRLSCrossTenantAccess`/`RLSMissingGUC`) reads `rls_violation_log` and reads/writes its own persisted high-water mark in `exporter_watermarks` (§4.2), both via `sysPool`, i.e. as `realm_provisioner_migrator`. Every privilege that role holds is provisioned by Terraform outside this repo (§10.1) — the domain migration grants it nothing on either table. Until that Terraform is updated to `GRANT SELECT ON rls_violation_log` and `GRANT SELECT, INSERT, UPDATE ON exporter_watermarks TO realm_provisioner_migrator`, this exporter fails every tick with a permission-denied error in any real environment (logged at WARN, not fatal — the previous counter value is retained, same as any other exporter query failure). `test/postgres/syspool_helper_test.go` grants both directly since the test harness doesn't run the real Terraform.

Full operator procedures for every headline scenario (Keycloak degraded, realm-config not landing, offboarding export failed, late conversion reversal, stuck idempotency key, pre-migration IdP audit, outbox/SQS/DLQ health, reconciler job health, emergency operator suspension) are in `docs/runbook.md`.
