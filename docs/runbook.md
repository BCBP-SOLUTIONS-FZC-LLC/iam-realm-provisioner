# Operator Runbook — iam-realm-provisioner

Full procedures for the headline scenarios listed in LLD §24. This service is the single writer to the Keycloak Admin API and the system of record for tenant identity lifecycle — most incidents here are either "Keycloak is unhappy" or "something didn't land locally after Keycloak succeeded." Grafana dashboard: "IAM — Realm Provisioner". Metric prefixes: `iam_realm_provisioner_*` (Tier 3, service-specific, frozen §25 — the business alerts read these) and `platform_*` (Tier 1, emitted by platform-gincommon/-events/-pgcommon plus this service's `platform_dependency_request_seconds{dependency="keycloak"}` — the HTTP, outbox and SQS alerts read these; alerts on registry-`proposed` metrics carry `metric_status: proposed`; see `.claude/operations.md` §11.1). Every series carries `{domain="iam", service="iam-realm-provisioner", environment}`. Alert rules: `deploy/monitoring/app-alerts.yml`.

---

## 1. Keycloak Admin API degraded

**Symptom:** `keycloak_call_failures_total` alert firing (rate > 5% for 5m), provisioning endpoints returning `502`/`realm_provisioning_failed`.

**Diagnosis:**
1. Check `iam_realm_provisioner_keycloak_call_duration_seconds{op}` and `iam_realm_provisioner_keycloak_call_failures_total{op}` broken down by operation — is it one op (e.g. `create_realm`) or everything? (The same calls are in `platform_dependency_request_seconds{dependency="keycloak",operation,outcome}` — `outcome="error"` for failures — for cross-service dependency views.)
2. Check `iam_realm_provisioner_admin_token_refresh_total{trigger="401"}` — a spike here means the admin credential was rotated (client secret changed in OpenBao/Keycloak) and the cached token is stale everywhere, not that Keycloak itself is down.
3. Check the client-side rate limiter budget (`KEYCLOAK_RATE_LIMIT_RPS`, default 20) — a burst of retries from callers can self-inflict 429s that look like a Keycloak outage.
4. Check Keycloak's own health/logs directly if reachable.

**Recovery:**
- **This service's provisioning flows are Keycloak-effect-first and idempotent.** No manual reconciliation is needed for a transient Keycloak outage — once Keycloak recovers, callers retry the same request with the same `Idempotency-Key` and the operation completes normally. Do not manually clear `provisioning_operations` rows to "unstick" a caller during an outage; let the retry mechanism work.
- If the root cause is credential rotation, verify the new secret is correctly written to OpenBao at `BAO_KEYCLOAK_ADMIN_PATH` (`iam/realm-provisioner/keycloak-admin`) and that this service has been restarted or its token cache naturally expires (see §6.1 Keycloak admin-token cache).
- If the root cause is the rate limiter, raise `KEYCLOAK_RATE_LIMIT_RPS` only after confirming Keycloak itself can absorb the increased load.

---

## 2. Realm-config not landing

**Symptom:** `realm_config_pending_gauge > 0` sustained for 30m+ (warning alert — RP-INV-6 risk: the stored config value and the actual Keycloak realm setting have diverged).

**Diagnosis:**
```sql
SELECT tenant_id, setting, status, last_error, updated_at
FROM realm_config_reconciliations
WHERE status = 'failed'
ORDER BY updated_at DESC;
```
Inspect `last_error` for each failing row — it is either a Keycloak-side error (permission, realm not found, malformed value) or a setting-validity issue on this service's side.

**Recovery:**
- The `realm-config-reconcile` job (RP-C4, runs every ~10 minutes per §11.1) automatically retries `pending` rows. A row that stays `failed` after several retry ticks needs manual intervention — fix the underlying Keycloak-side issue (e.g. grant a missing permission, correct a malformed realm-config value at the source) and let the next tick retry it, or re-run the job directly:
  ```bash
  make run-reconciler JOB=realm-config-reconcile
  ```
- **A persistent failure here is a known, flagged divergence — never a silent one** (RP-INV-6). Do not manually UPDATE the realm-config row to `applied` unless you have independently confirmed the Keycloak realm actually reflects the setting; doing so would hide a real divergence rather than fix it.

---

## 3. Offboarding export failed

**Symptom:** `realm_export_total{result="fail"}` — this is a **critical** alert (compliance-blocking).

**Diagnosis:** Check S3/KMS access from this service's pod — `REALM_EXPORT_BUCKET` and `REALM_EXPORT_KMS_KEY` (env vars). Common causes: IAM policy drift (see `deploy/iam/policy.json`), KMS key policy not granting this service's role `kms:GenerateDataKey`/`kms:Decrypt`, or the bucket having been renamed/deleted.

**Recovery:**
- **The tenant realm is NOT deleted until a confirmed export succeeds** (RP-EVT-4) — the tenant stays in `suspended` state, so there is no data-loss risk from this failure by design. There is no urgency to "unblock" the tenant record; the urgency is fixing the export path.
- Fix the S3/KMS access issue, then re-run the offboarding trigger for the affected tenant (whatever kicked off the original export — the lapse sweep, or an operator-triggered offboard). The realm is deleted only after the corresponding `TenantOffboarded` event with a populated `export_s3_key` has been produced.

---

## 4. Late conversion reversal (> 7 days)

**Symptom:** `POST .../tenants/:id/revert-conversion` returns `409 revert_window_expired` with `details.converted_at`.

**Recovery:** This is by design — reversal past the 7-day window is not a self-service API operation. The manual procedure:
1. Recreate the shared-realm tenant row (the trial/shared-realm identity that existed before conversion).
2. Migrate the tenant's users back from the dedicated realm to the shared realm, using the same `KeycloakAdmin.MigrateUser` primitive the forward conversion path used (global user-ID uniqueness still applies — the dedicated-realm user must be deleted before recreating it in the shared realm).
3. Tear down the dedicated realm (`KeycloakAdmin.DeleteRealm`) only after every user has been confirmed migrated back.
4. Emit a corrective audit trail entry — this reversal happens outside the normal event-producing code path, so there is no automatic `TenantConverted`-reversal event; document the action out-of-band (audit log / ticket) per RP-8.

This is intentionally an operator-run manual procedure, not an API endpoint — it is rare, higher-risk than the forward path, and benefits from a human confirming state at each step.

---

## 5. Stuck `in_progress` idempotency key

**Symptom:** A caller retries a request and keeps getting `409 operation_in_progress`.

**This now resolves on its own.** Two causes used to leave a key `in_progress` forever, and both are fixed:
- `Fail`/`Complete` ran on the request context, so a request cut off by the 30s deadline or a client disconnect could not record its outcome. They now run detached from it (`IdempotencyRepository`).
- A crash or SIGKILL mid-request left nothing to record the outcome. `Begin` now **reclaims an `in_progress` row older than the lease** (`IDEMPOTENCY_IN_PROGRESS_LEASE`, 15m). The next retry with the same key re-runs the operation, and every step is a Keycloak-effect-first reconcile, so an already-applied step replays as a no-op.

A `409` within the lease is expected: the original execution may still be running. RP-3 conversions in particular continue detached for up to `CONVERT_TIMEOUT` (10m) after the caller's request times out, and the retry replays the result once they finish.

**Diagnosis** (keys stuck past the lease mean something else is wrong — e.g. the clock or the lease setting):
```sql
SELECT idempotency_key, operation, tenant_id, status, created_at
FROM provisioning_operations
WHERE status = 'in_progress' AND created_at < now() - interval '15 minutes';
```

**Manual recovery** (only if a key must be released before the lease expires): first confirm in Keycloak that the realm/user/IdP the operation was creating is not half-configured — prefer fixing the local row to match Keycloak over deleting Keycloak state. Then:
```sql
UPDATE provisioning_operations SET status = 'failed', completed_at = now()
WHERE idempotency_key = '<key>' AND status = 'in_progress';
```
The caller's next retry with the **same** key then re-executes (a `failed` key is always reclaimed).

---

## 6. Pre-migration IdP portability audit

**Symptom / trigger:** Ahead of a planned regional cutover, an operator needs assurance that no tenant's IdP configuration has a region-portability problem (HLD §11.6).

**Procedure:**
```bash
curl -X POST https://<host>/api/v1/internal/audit/idp-region-portability
```
This is RP-13. The report must show zero failing tenants (or documented waivers for any WARN-level acknowledgement) before proceeding with the cutover. If `failing_tenants` is non-empty, resolve each tenant's IdP configuration against the §11.6 checklist (CRITICAL checks 0-2/4-6/10-12/14-15 block outright; WARN checks 7-9/13 require a recorded admin acknowledgement) before re-running the audit.

Note: as of this build, an empty `failing_tenants` array serializes as JSON `null` rather than `[]` — a cosmetic gap, not a correctness issue; treat `null` and `[]` as equivalent ("no failing tenants") when reading the report.

---

## 7. Outbox relay lag / SQS consumer stuck / DLQ buildup

Not a numbered LLD headline item, but a common operational need given the outbox + 3-consumer architecture.

**Outbox relay lag:**
- Check `platform_dlq_messages_total{operation="outbox_publish",reason="max_attempts"}` (from `platform-events`, by `event_type`) — an increase over 30m is a warning alert (`IAMRealmProvisionerOutboxStuck`); `platform_outbox_pending_events` and `platform_outbox_errors_total{operation}` show backlog and poll/mark-published failures. Rows land in `outbox_dead_letters` after `OUTBOX_MAX_ATTEMPTS` (default 5) failed publish attempts.
- Check `OUTBOX_POLL_INTERVAL` (default 500ms) and `OUTBOX_BATCH_SIZE` (default 50) — under sustained high write volume, a too-small batch size can cause the relay to fall behind; check whether unpublished `outbox_events` rows are accumulating faster than the relay drains them.
- A dead-lettered event needs manual inspection of its payload and `last_error`, then either a republish or acceptance that the event is unrecoverable (document why — every produced event has downstream consumers). There is **no endpoint or job for this** — `outbox.Runner` exposes `ListDeadLetters`, `ReprocessDeadLetters`/`ReprocessDeadLettersWith`, and `DiscardDeadLetters`, but this service wires none of them (nor do the sibling services), so today the options are a direct `SELECT` against `outbox_dead_letters` for inspection and a manual re-INSERT into `outbox_events` to republish. If DLQ dwell becomes routine rather than exceptional, wiring `ReprocessDeadLettersWith` behind an operator endpoint is the intended fix.

**Stuck SQS consumer / DLQ buildup:**
- Each inbound queue (`membership-realm-q`, `tenant-realm-q`, `billing-realm-q`) has its own `-dlq` with `maxReceiveCount=5`. DLQ depth > 0 is a warning alert.
- A message lands in the DLQ after 4 failed deliveries — the handler sees deliveries 1–4 and platform-events forwards the 5th to `-dlq` itself (`platform_dlq_messages_total`, DLQ message attribute `DLQReason`) just before SQS's own redrive would; a consumed-schema violation (`DLQReason=schema_violation`) is forwarded on its **first** delivery — inspect the message body and this service's logs (structured, includes `tenant_id`/`operation`/`trace_id`) for the actual error. If `iam_realm_provisioner_consumed_schema_violations_total{consumer,event_type}` rose too (critical alert `IAMRealmProvisionerConsumedSchemaViolation`, log line `consumed event violates its consumed-message schema`), the payload failed the consumer's consumed-message schema (`internal/eventschema`) before reaching the handler: either the producer broke its contract, or our `api/asyncapi.yaml` is stale/stricter than what it sends — fix that side (`make extract-schemas` after an asyncapi edit), then redrive. Other common causes: a malformed payload from the producer, a downstream dependency (e.g. Org & Membership for the I-8 residual-MFA check) being unavailable, or a genuine bug in the consumer handler.
- Once the root cause is fixed, messages can be manually redriven from the DLQ back to the source queue (standard SQS redrive, `aws sqs start-message-move-task` or console redrive) — this service's consumers are safe to reprocess a message because of `processed_events` dedup (see below).

**Manually replaying a `processed_events`-deduped event:**
- If a message was correctly processed but its downstream effect needs to be re-triggered for some other operational reason (rare — e.g. verifying an effect after a suspected partial failure), find and delete its row from `processed_events` first, then redrive/republish the message:
  ```sql
  DELETE FROM processed_events
  WHERE consumer = '<membership|tenant|billing>' AND event_id = '<event-id>';
  ```
  Do this only when you have a specific reason to believe the original processing did not fully take effect (e.g. a downstream write rolled back after the event was recorded as processed) — replaying a genuinely-already-applied event is safe for most handlers (they're idempotent upserts) but should still be a deliberate action, not routine.

---

## 8. Reconciler job health (RP-C1..RP-C4 + retention jobs)

Six jobs run via `cmd/reconciler --job=<name>`, each as its own K8s CronJob:

| Job | LLD ref | Purpose | Health signal |
|---|---|---|---|
| `trial-expiry` | RP-C1 | Soft-expires trials past `trial_ends_at` + `TRIAL_GRACE_DAYS` | `iam_realm_provisioner_trial_expiry_processed_total`, `iam_realm_provisioner_trial_expiry_deferred_total` (should stay near 0 — sustained >0 for 1h is a warning alert) |
| `trial-hard-delete` | RP-C2 | Hard-deletes trial users and the tenant's `platform-automation-<tenant_id>` client from the shared realm past `TRIAL_GRACE_DAYS` after expiry, then scrubs the row | Check job's own exit code / K8s CronJob history; no dedicated metric beyond the shared provision/lapse counters |
| `orphaned-signup-reap` | §8.7 (BFF gap A-3) | Reaps signups that never received `TrialStarted` after `ORPHAN_SIGNUP_AFTER` (48h): hard-deletes the shared-realm user(s) and automation client, revokes the Token Service principal (TS-7), scrubs + soft-deletes the row, so the email can sign up again | `iam_realm_provisioner_orphaned_signup_reap_total{result}` — `reaped` is expected at a low rate (abandoned signups); sustained `deferred` means Keycloak, Token Service or Postgres trouble (or `TOKEN_SERVICE_BASE_URL` unset on the CronJob). A log error `TS-7 returned 404 without PRINCIPAL_NOT_FOUND` means the Token Service in this environment predates TS-7 — deploy Token Service first; nothing was reaped, the next run retries; any `skipped` means a signup completed only after `ORPHAN_SIGNUP_AFTER` — check O&M's DLQ/backstop. A `WARN tenant consumer: TrialStarted for an erased tenant` log means the reap beat a very late I-1: O&M now has a tenant with no identity — offboard it in O&M. |
| `subscription-lapse` | RP-C3 | Reads subscription status and suspends/offboards lapsed tenants | `iam_realm_provisioner_lapse_processed_total{transition=suspend\|offboard}` — **see the open item below** |
| `realm-config-reconcile` | RP-C4 | Retries `pending`/`failed` realm-config rows | `iam_realm_provisioner_realm_config_pending_gauge` (see scenario 2 above) |
| `outbox-prune` | — | Deletes published `outbox_events` older than `OUTBOX_RETENTION_DAYS` (default 8) | Check K8s CronJob history; a growing `outbox_events` table row count is the symptom of this job not running |
| `processed-events-prune` | — | Deletes `processed_events` rows older than `PROCESSED_EVENTS_TTL_DAYS` (default 8) | Same as above, for `processed_events` |

General health check for any job: K8s CronJob's `lastScheduleTime`/`lastSuccessfulTime` and pod logs. All jobs accept `RECONCILER_BATCH_LIMIT` (default 100) and `RECONCILER_TIMEOUT` (default 5m) — a job repeatedly timing out at the batch limit under a large backlog may need a temporarily larger `RECONCILER_BATCH_LIMIT` or more frequent scheduling, not a code change.

**Instrumentation note:** every reconciler job must *increment* its `*_deferred_total` counter on every defer, not merely register it at startup — otherwise the "deferred > 0 sustained" alert can never fire even when the job is genuinely stuck deferring the same rows repeatedly. Verify this behavior if a new deferred-style job is added.

---

## 9. Emergency operator suspension (RP-14)

**Procedure:** `POST .../tenants/:id/suspend` with `source=operator`. This disables the tenant's realm in Keycloak immediately (login blocked) and produces `TenantSuspended{source: "operator"}`. Use for security incidents (compromised tenant, abuse) rather than billing-driven lapses, which flow through `subscription-lapse` instead. Reactivate via `POST .../tenants/:id/reactivate` (RP-10) with the matching `source`.

---

## 10. Pre-deploy check: tenants past the retention window (CD-23)

**When:** before **every deployment to any environment**, and before **any change to `RETENTION_DAYS`** (Helm `config.RETENTION_DAYS`, code default 60 in `cmd/reconciler/main.go`). This is a standing step, not a one-off for the CD-23 90 → 60 change (that query previously lived only in the LLD 1.28 changelog row).

**Why:** the offboard leg of `subscription-lapse` (RP-C3, LLD §8.8) exports, deletes and scrubs every billing-lapse-suspended realm whose `disabled_at` is older than `RETENTION_DAYS` — it is **terminal** (RP-INV-5). Shortening the window, or deploying to an environment whose suspended tenants have aged while the job wasn't running, makes every matching tenant eligible on the **next** run. Know who they are before that happens.

**Query** (run against the `provisioner` database with the `RETENTION_DAYS` value that will be live after the deploy — it is the same predicate `ListSuspendedPastRetention` uses):

```sql
SELECT tenant_id, disabled_at FROM tenant_realms
WHERE status = 'suspended' AND suspension_source = 'billing_lapse'
  AND disabled_at < now() - interval '<RETENTION_DAYS> days'
ORDER BY disabled_at;
```

**Reading the result:**
- **0 rows** — proceed.
- **Any rows** — those tenants will be offboarded (realm exported to S3, then deleted) on the first `subscription-lapse` run after the deploy. Before deploying, confirm with Billing/Commercial that each one is genuinely due for offboarding; for any that is not, reactivate it (Billing's `TenantReactivated` on `billing-realm-q`, or RP-10 for an operator decision) or hold the deploy. Operator suspensions (`suspension_source='operator'`) are never auto-offboarded and are deliberately excluded.

`scripts/pre-deploy-check.sh` runs this query automatically when `PROVISIONER_DATABASE_URL` is set (with `RETENTION_DAYS`, default 60) and fails if any rows match.

---

## Open items — operational impact

- **RP-C3's suspend phase needs `ORG_MEMBERSHIP_BASE_URL` set.** The `subscription-lapse` job polls O&M's I-16 `GET /api/v1/internal/subscription-lapses` via `port.SubscriptionLapseReader` (`internal/adapter/outbound/orgmembership`) for its suspend phase. If `ORG_MEMBERSHIP_BASE_URL` is unset, the job does **not** fail — it logs a WARN, skips the suspend phase, and still runs its offboard phase (retention-driven, purely local against `suspension_source='billing_lapse'` rows). Check `iam_realm_provisioner_lapse_processed_total{transition="suspend"}` staying at zero alongside a healthy `lastSuccessfulTime` as the signal this env var is missing in a deployment where it should be set.
- **OpenBao integration is untested outside a real Kubernetes cluster.** The Kubernetes ServiceAccount JWT the auth flow depends on (`/v1/auth/kubernetes/login`) only exists inside a real cluster with the correct service account bound (see `deploy/openbao/role.tf.example`). In any local-dev or CI environment without that binding, RP-6 (configure IdP)/RP-7 (delete IdP) will return `500` on the OpenBao write/delete step. This is expected local-dev/CI behavior, not a production defect — verify OpenBao connectivity as part of any staging/production rollout checklist before relying on IdP configuration endpoints.
