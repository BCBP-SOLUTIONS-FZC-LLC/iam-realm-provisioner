# Runbooks: iam-realm-provisioner alerts

Runbooks for every alert in [`deploy/monitoring/app-alerts.yml`](../../deploy/monitoring/app-alerts.yml), [`deploy/monitoring/slo-rules.yml`](../../deploy/monitoring/slo-rules.yml) and the Helm [`PrometheusRule`](../../deploy/helm/templates/prometheusrule.yaml). The heading is the alert name, and the alert's `runbook` annotation (`docs/observability/runbooks.md#<alert-name-lowercase>`) points to the same entry. Every alert carries `domain="iam"`, `service="iam-realm-provisioner"` and `environment`. The Glue schema-registry alerts (`deploy/monitoring/schema-registry-alerts.yml`) are covered by [`../runbook-schema-registry.md`](../runbook-schema-registry.md); platform-level alerts on the libraries' own metrics are documented in platform-gincommon's `docs/observability/runbooks.md`. Longer incident procedures are in [`../runbook.md`](../runbook.md).

Common starting points:

- Rollouts: `count by (version) (platform_build_info{service="iam-realm-provisioner",environment="$environment"})`
- Logs (Loki): `{service="iam-realm-provisioner"} | json`, then follow `trace_id` into Tempo. Keycloak spans are `keycloak.<op>`, consumer spans `consumer.<event_type>`, reconciler runs `reconciler.<job>`.
- Readiness: `GET /readyz` reports Postgres (app pool and sysPool), the outbox runner and the Keycloak admin-token bootstrap.
- Reconciler metrics come from the Pushgateway (`cronjob` label). If `PROMETHEUS_PUSHGATEWAY_URL` is empty, the reconciler alerts below can never fire.

## Availability

### IAMRealmProvisionerDown

**Meaning.** No pod has been scraped as healthy for 2 minutes. Every provisioning, conversion, IdP, MFA and session operation in the platform goes through this service (RP-INV-1), so signup, O&M invites and Billing conversions are all failing.

**Triage.**
1. `kubectl get pods -l app.kubernetes.io/name=iam-realm-provisioner` and `kubectl describe pod` for crash loops and probe failures.
2. Startup panics are deliberate for bad configuration, and the message names it: a required env var missing or still holding its dev placeholder (`KEYCLOAK_BASE_URL`, `KEYCLOAK_ADMIN_CLIENT_SECRET`, `ORG_MEMBERSHIP_BASE_URL`, `TOKEN_SERVICE_BASE_URL`), an invalid `APP_ENV`, `DLQ preflight` (an inbound queue without a RedrivePolicy), or an unregistered/not-`AVAILABLE` Glue schema definition.
3. Schema: the migrate hook Job owns migrations (`RUN_MIGRATIONS=false` on replicas). A failed `…-migrate` Job fails the release before pods roll; `kubectl logs job/<release>-migrate` names a dirty migration or a schema newer than the image.

**Mitigate.** Roll back the last deploy, or fix the named configuration.

### IAMRealmProvisionerReplicasMissing

**Meaning.** Fewer than 2 replicas are healthy. One more disruption takes the service down, and node drains are blocked by the PDB.

**Triage.** As for [`IAMRealmProvisionerDown`](#iamrealmprovisionerdown), per pod. Check node pressure and pending pods.

## HTTP errors and latency

### IAMRealmProvisionerHighErrorRate

**Meaning.** More than 5% of requests returned 5xx over 5 minutes.

**Triage.**
1. Which routes and codes: `sum by (route, error_class) (rate(platform_http_requests_total{service="iam-realm-provisioner",status_class="5xx"}[5m]))`.
2. The error body's `error` code tells the cause:
   - `keycloak_unavailable` (502): Keycloak. See [`IAMRealmProvisionerKeycloakCallFailureRate`](#iamrealmprovisionerkeycloakcallfailurerate).
   - `conversion_failed` (502): an RP-3 trial→dedicated conversion failed mid-migration and was compensated (`bestEffortDeleteRealm`). Logs carry the failing user and step.
   - `db_unavailable` (503): Postgres or PgBouncer. Check `platform_db_pool_connections`, `platform_db_pool_empty_acquires_total`, and RDS/PgBouncer health.
   - `token_service_unavailable` / `org_membership_unavailable` / `object_store_unavailable` (503): the named dependency.
   - `internal_error`: an unhandled error, logged as `unhandled 500 error`.
3. Request timeouts (30s deadline) appear on `platform_http_request_timeouts_total`.

**Mitigate.** Roll back a bad version, or restore the failing dependency. Callers retry with the same `Idempotency-Key`, which replays or resumes safely.

### IAMRealmProvisionerCriticalErrorRate

**Meaning.** More than 20% of requests returned 5xx. Same triage as [`IAMRealmProvisionerHighErrorRate`](#iamrealmprovisionerhigherrorrate); at this level it is almost always Keycloak or Postgres being down, or a bad deploy.

### IAMRealmProvisionerHighLatency

**Meaning.** p99 across all routes is above 1s for 10 minutes. Most routes wrap one or more Keycloak Admin API calls, so this usually tracks Keycloak.

**Triage.** `histogram_quantile(0.99, sum by (le, route) (rate(platform_http_request_duration_seconds_bucket{service="iam-realm-provisioner"}[5m])))` to find the route. Then:
- Keycloak latency: `histogram_quantile(0.99, sum by (le, op) (rate(iam_realm_provisioner_keycloak_call_duration_seconds_bucket[5m])))`;
- the client-side rate limiter (`KEYCLOAK_RATE_LIMIT_RPS`) queueing during a burst (e.g. an RP-3 conversion migrating many users — its p95 budget is 8s + ~0.3s/user, so a single conversion can move p99 legitimately);
- slow queries (`platform_db_slow_queries_total`) and pool waits (`platform_db_pool_acquire_duration_seconds`).

## Keycloak

### IAMRealmProvisionerKeycloakCallFailureRate

**Meaning.** More than 5% of Keycloak Admin API calls are failing (Tier 3 `iam_realm_provisioner_keycloak_call_failures_total` over `_keycloak_call_duration_seconds_count`). The adapter already retries 5xx/timeouts with backoff and honours 429 `Retry-After`, so these are failures after retries.

**Triage.** `sum by (op) (rate(iam_realm_provisioner_keycloak_call_failures_total[5m]))` — one `op` (e.g. `create_realm`) points at a realm-level problem, all ops at Keycloak itself or the network. `op="token"` means the admin credential is failing. Check Keycloak health and the shard (`keycloak_shard`) involved. See [`../runbook.md` §1](../runbook.md#1-keycloak-admin-api-degraded).

**Mitigate.** Restore Keycloak. Synchronous callers got `502 keycloak_unavailable` and retry; realm-config changes queue in `realm_config_reconciliations` (RP-C4 converges them); trial expiry defers to the next run.

### IAMRealmProvisionerAdminTokenRefresh401Spike

**Meaning.** More than 3 admin-token refreshes in 15 minutes were triggered by a 401 rather than proactively. The cached admin token is being rejected — almost always a rotated `KEYCLOAK_ADMIN_CLIENT_SECRET`, a changed role on the admin service account, or a restored/reset Keycloak.

**Triage.** Compare the secret in OpenBao (`BAO_KEYCLOAK_ADMIN_PATH`, delivered by ESO) with the client secret in Keycloak; check the service account still holds `create-realm` (master) and `realm-admin` on managed realms. Look for `keycloak.token` spans with errors.

**Mitigate.** Re-sync the secret (bump `rotationEpoch` to roll pods after an ESO update) or restore the role.

## Lifecycle and reconcilers

### IAMRealmProvisionerTrialExpiryDeferredSustained

**Meaning.** The `trial-expiry` CronJob (RP-C1) deferred rows for an hour: `DisableUsersInRealm` or the local commit failed for some expired trials, which are retried on the next run. Pushgateway metric.

**Triage.** Job logs (`trial-expiry: defer` with `tenant_id` and the error); Keycloak health for the shared `trial` realm; `provisioning_operations` rows for `trial-expiry:<tenant>:…` keys stuck `in_progress` (see [`../runbook.md` §5](../runbook.md#5-stuck-in_progress-idempotency-key)).

**Mitigate.** Fix the cause; the next daily run converges. See [`../runbook.md` §8](../runbook.md#8-reconciler-job-health-rp-c1rp-c4--retention-jobs).

### IAMRealmProvisionerRealmConfigPendingSustained

**Meaning.** `realm_config_reconciliations` has had `pending` rows for 30 minutes: RP-8 accepted a realm-config change (`200 {applied:false, reconciling:true}`) that Keycloak has not taken yet, and RP-C4 (every 10 min) is not converging it.

**Triage.** `SELECT tenant_id, setting_key, attempts, last_error FROM realm_config_reconciliations WHERE status='pending' ORDER BY created_at LIMIT 20;` (sysPool). Rows reaching `RECONCILER_MAX_ATTEMPTS` (20) move to `failed` and need an operator. See [`../runbook.md` §2](../runbook.md#2-realm-config-not-landing).

### IAMRealmProvisionerSharedRealmNotConverged

**Meaning.** The `ensureSharedRealm` background loop (`cmd/server/shared_realm.go`) has not set `iam_realm_provisioner_shared_realm_ready` to 1 within the first 5 minutes after server startup. The shared trial realm (SHARED_TRIAL_REALM) is either missing or not fully configured, so RP-1 trial provisioning is failing.

**Triage.** Server logs for `"ensure shared trial realm failed"` with the specific error; Keycloak Admin API health from the pod (`kubectl exec`); confirm `SHARED_TRIAL_REALM`, `KEYCLOAK_BASE_URL`, and admin credentials are set. If the realm exists in Keycloak but convergence fails, the specific Keycloak operation in the error identifies which invariant (client, mapper, role, MFA flow) is drifting.

**Mitigate.** Fix the underlying Keycloak or configuration issue; the loop retries every 30 s and sets the gauge to 1 on the next success. See [`../runbook.md` §1](../runbook.md#1-realm-template-drift).

### IAMRealmProvisionerPlatformRealmNotConverged

**Meaning.** The `ensurePlatformRealm` background loop (`cmd/server/platform_realm.go`, CD-52 item 2) has not set `iam_realm_provisioner_platform_realm_ready` to 1 within the first 5 minutes. The platform operator realm (PLATFORM_REALM_NAME) is missing or its invariants (platform_operator role, realm-wide MFA, short sessions) are not converged. Platform operator login and AuthZ Enrichment's `platform_operator` role injection will not work.

**Triage.** Server logs for `"ensure platform realm failed"`; Keycloak Admin API health; confirm `PLATFORM_REALM_NAME`, `KEYCLOAK_BASE_URL`, and admin credentials are correct. The error message names the specific invariant that failed (create realm, create role, set OTP flow, set session settings).

**Mitigate.** Fix the underlying issue; the loop retries every 30 s. This alert is severity=warning because tenant realm flows are unaffected; only operator-facing routes that require the platform realm are degraded. This alert only fires when `PLATFORM_REALM_NAME` is set (CD-52 OP-G3); it is absent in pre-CD-52 environments.

### IAMRealmProvisionerRealmExportFailure

**Meaning.** RP-C3's offboard phase failed to export a realm to S3. By RP-EVT-4 the realm is never deleted without a confirmed export, so the tenant stays `suspended` — no data is lost, but offboarding is stuck. Pushgateway metric.

**Triage.** `subscription-lapse` job logs; S3 bucket (`REALM_EXPORT_BUCKET`) and KMS key (`REALM_EXPORT_KMS_KEY`) permissions; Keycloak's realm export for that realm. See [`../runbook.md` §3](../runbook.md#3-offboarding-export-failed).

**Mitigate.** Fix the cause; the next run retries. Never delete the realm by hand without an export.

## Outbox and queues

### IAMRealmProvisionerOutboxStuck

**Meaning.** Produced tenant-lifecycle events (`TenantIdentityProvisioned`, `TenantRealmReady`, `TenantConverted`, ...) exhausted `OUTBOX_MAX_ATTEMPTS` (5) and moved to `outbox_dead_letters`. O&M, AuthZ Enrichment and Audit Log never received them, so their projections are behind this service.

**Triage.**
1. `SELECT id, event_type, tenant_id, attempts, last_error, failed_at FROM outbox_dead_letters ORDER BY failed_at DESC LIMIT 20;`
2. `last_error` usually names the cause: SNS permission, `SNS_TOPIC_TENANT_ARN`, or a Glue schema version not `AVAILABLE`.
3. Transient SNS failures don't count an attempt; a backlog without dead letters shows on `platform_outbox_oldest_pending_age_seconds` instead.

**Mitigate.** Fix the cause, then replay with `outbox.Runner.ReprocessDeadLetters`. See [`../runbook.md` §7](../runbook.md#7-outbox-relay-lag--sqs-consumer-stuck--dlq-buildup).

### IAMRealmProvisionerSQSReceiveErrors

**Meaning.** `ReceiveMessage` on an inbound queue is failing (`platform_dependency_request_seconds_count{dependency="sqs",operation="receive_message",outcome="error"}`). The affected consumer is not applying O&M membership, tenant or Billing events.

**Triage.** Pod logs (`SQS consumer stopped`); IAM permissions (`sqs:ReceiveMessage`, `GetQueueUrl`, `GetQueueAttributes`, `SendMessage` on the `-dlq`); queue URLs (`SQS_*_REALM_QUEUE_URL`).

**Mitigate.** Fix permissions or networking; restart the pod if the consumer goroutine stopped.

### IAMRealmProvisionerInboundDLQDepth

**Meaning.** Messages are sitting in an inbound queue's DLQ (`membership-realm-q-dlq`, `tenant-realm-q-dlq` or `billing-realm-q-dlq`). They got there through platform-events' forwarding: a consumed-schema violation on its first delivery, a malformed body, or a handler that failed four times.

**Triage.** Inspect a DLQ message's `DLQReason` (`schema_violation`, or the library's description for a receive-count, malformed or decode forward), `OriginalQueue` and `FailedAt` attributes. `sum by (operation, reason) (increase(platform_dlq_messages_total{service="iam-realm-provisioner"}[1h]))` shows the inflow by metric reason (`explicit` = schema violation, `max_receive_count`, `malformed`, `decode_error`). This alert reads the CloudWatch exporter's `aws_sqs_approximate_number_of_messages_visible_average`; platform-events' own `platform_dlq_depth` (proposed) carries the same signal from inside the service.

**Mitigate.** Fix the cause (producer contract, dependency, bug), then redrive. Never discard without the owning team's sign-off.

### IAMRealmProvisionerUnknownEventType

**Meaning.** A producer emits an event type this consumer has no handler for (`{topic,event_type}`). It is acknowledged and claimed in `processed_events` (forward compatibility), so nothing is lost but nothing is applied.

**Triage.** `sum by (topic, event_type) (rate(iam_realm_provisioner_unknown_event_acknowledged_total[1h]))`.

**Mitigate.** If the type matters to this service, add a case to the consumer in `internal/adapter/inbound/consumer/` and its consumed schema (`api/asyncapi.yaml`, `make extract-schemas`). Otherwise, narrow the SNS subscription filter.

### IAMRealmProvisionerConsumedSchemaViolation

**Meaning.** Consumed `{event_type}` payloads on `{consumer}` fail this service's embedded consumed-message schema (`internal/eventschema`). Each one is forwarded to the DLQ on its first delivery (message attribute `DLQReason=schema_violation`; counted as `platform_dlq_messages_total{reason="explicit"}`) without reaching the handler, so every event of that type from that producer is being dropped.

**Triage.** Compare a DLQ copy with `api/asyncapi.yaml` and `internal/eventschema/`. Either the producer broke its contract, or this repo's copy of the consumed schema is stale or stricter than what the producer sends. Remember `billing-realm-q`'s `TenantReactivated` is Billing's, not this service's own (RP-D13).

**Mitigate.** Fix the wrong side (`make extract-schemas` after an asyncapi edit), then redrive. See [`../runbook-schema-registry.md`](../runbook-schema-registry.md).

## Platform libraries and audit delivery

These alerts read platform-library metrics that are still `proposed` in the registry (labelled `metric_status: proposed`), except the two audit alerts, which read this service's Tier 3 gauge.

### IAMRealmProvisionerOutboxBacklog

**Meaning.** The oldest unpublished outbox event is more than 5 minutes old (`platform_outbox_oldest_pending_age_seconds`). Transient SNS failures (throttling, outage, an expired credential) never count an attempt, so they never dead-letter — they show up here instead of on [`IAMRealmProvisionerOutboxStuck`](#iamrealmprovisioneroutboxstuck).

**Triage.** SNS errors in the outbox runner logs; `platform_outbox_pending_events`; `platform_dependency_request_seconds_count{dependency="sns",outcome="error"}`; whether the runner is running at all (`/readyz` reports it); IRSA permissions on `SNS_TOPIC_TENANT_ARN`.

### IAMRealmProvisionerDLQBacklog

**Meaning.** Messages have sat in an inbound queue's DLQ for 30 minutes (`platform_dlq_depth`, sampled every `SQS_QUEUE_DEPTH_INTERVAL`). The inflow alerts ([`IAMRealmProvisionerConsumedSchemaViolation`](#iamrealmprovisionerconsumedschemaviolation)) say why they arrived; this one says they are still there. Same signal as [`IAMRealmProvisionerInboundDLQDepth`](#iamrealmprovisionerinbounddlqdepth), from inside the service rather than CloudWatch.

**Mitigate.** Inspect the `DLQReason` attribute, fix the cause, then redrive. Never discard without the owning team's sign-off.

### IAMRealmProvisionerConsumerDead

**Meaning.** No `ReceiveMessage` call has been made for 10 minutes. The consumers long-poll every ~20s even when their queues are idle, so this means the consumer goroutines have stopped — not that the queues are quiet. MFA role sync, trial timers and Billing reactivations are not being applied.

**Triage.** Pod logs (`SQS consumer stopped`, a panic); `kubectl get pods` restarts; whether the queue URLs are set (an empty URL disables that consumer at startup with a WARN); IAM permissions.

**Mitigate.** Restart the pods if the consumers do not recover, then fix the root cause.

### IAMRealmProvisionerDBPoolSaturated

**Meaning.** More than 90% of a pool's connections have been checked out for 10 minutes on the busiest replica. `pool="default"` is the API pool; `pool="system"` the BYPASSRLS pool (reconciler sweeps, gauge exporters, the platform-audit outbox Runner, RP-13).

**Triage.** `platform_db_pool_acquire_duration_seconds` (are callers waiting?), slow queries (`platform_db_slow_queries_total`), long transactions; whether traffic or HPA scale-out (replicas × `PG_MAX_CONNS` against PgBouncer) changed.

### IAMRealmProvisionerDBAcquireErrors

**Meaning.** More than 5% of connection acquires fail on the worst replica (timeouts, a closed pool, Postgres/PgBouncer unreachable). Requests answer `503 db_unavailable`.

**Triage.** PgBouncer and RDS health; `platform_db_pool_connections{state}`; pod logs for connection errors; the NetworkPolicy's `databasePorts`.

### IAMRealmProvisionerDBRetriesExhausted

**Meaning.** Transactions kept hitting deadlocks or serialization failures through every pgcommon retry.

**Triage.** `reason` and `pool`; Postgres deadlock logs. Two transactions locking the same rows in opposite order is the usual cause.

### Audit delivery (platform-audit)

The service's own `IAMRealmProvisionerAuditEntriesRejected` / `…Undelivered` alerts and the `iam_realm_provisioner_audit_entry_*` metrics were retired with platform-audit release N+1. Audit delivery is monitored by the library's rules (`deploy/helm/templates/prometheusrule-platform-audit.yaml`) on `platform_audit_*`, and runbooks are the library's ([platform-audit runbook](https://github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/blob/main/docs/observability/runbook.md)):
- **Parked rows** (`PlatformAuditEntriesParked`): `SELECT id, tenant_id, entry->>'entry_type' AS entry_type, last_outcome, last_error, failed_at FROM pending_audit_entries WHERE status='failed'`. Never `UPDATE` these rows by hand: the library's constraints tie `status` to `failed_at`, and every disposition must be audited through platform-audit's operator helpers (`auditoutbox.Requeue` / `Discard` / `Resolve`).
- **Undelivered rows** (`PlatformAuditDeliveryStalled`, `PlatformAuditNotAdmitted`, `PlatformAuditUnknownEntryType`, `PlatformAuditDeliveryDisabled`): Audit Log unreachable, not admitting this service, not knowing the entry type yet, or `AUDIT_LOG_BASE_URL` unset. Nothing is lost; the backlog drains once the cause is fixed.

## Tenant isolation

### IAMRealmProvisionerRLSCrossTenantAccess

**Meaning.** RLS-6: a query read or wrote a row whose `tenant_id` did not match the transaction's `app.tenant_id` GUC. The log is sampled (1%), so a sampled hit means more occurred. This is either an attack or a serious application bug.

**Triage.** Query `rls_violation_log` (sysPool / migrator role) for `violation_type='cross_tenant_access'`; the row carries `table_name`, `row_tenant_id`, `app_tenant_id` and `query_text`. The exporter needs a Terraform-side `GRANT SELECT ON rls_violation_log TO realm_provisioner_migrator` (`.claude/operations.md` §20) — without it this alert can never fire.

**Mitigate.** Page security. Identify the code path from `query_text` and the trace, and roll back if a recent change introduced it.

### IAMRealmProvisionerRLSMissingGUC

**Meaning.** Queries ran without a valid `app.tenant_id` GUC (`violation_type="missing_or_invalid_guc"`). RLS returns zero rows rather than leaking data, but the request silently sees nothing.

**Triage.** `query_text` in `rls_violation_log` identifies the path. Typical causes: a repository call outside `GUCBridgeMiddleware`, a consumer path missing `withTenantGUC`, or a cross-tenant sweep run on the app pool instead of the sysPool (e.g. `SYSTEM_DATABASE_URL` unset, which logs a startup warning).

## SLOs

### IAMSLO1_RealmProvisionerWriteErrorFastBurn

**Meaning.** More than 7.2% (14.4× the 0.5% budget) of mutating requests (`POST|PUT|PATCH|DELETE`) returned 5xx over 5 minutes. Pages.

**Triage.** As for [`IAMRealmProvisionerHighErrorRate`](#iamrealmprovisionerhigherrorrate), restricted to write routes. Check whether the error is `keycloak_unavailable`, `db_unavailable` or a fail-closed dependency, and check `/readyz`.

### IAMSLO1_RealmProvisionerWriteErrorSlowBurn

**Meaning.** The write-path 5xx rate has been above 3% (6× budget) for 15 minutes. Ticket. Same triage as the fast burn; look for one route or tenant shard degrading slowly.

### IAMSLO2_RealmProvisionerKeycloakFastBurn

**Meaning.** The Keycloak Admin API failure ratio is above 14.4% on both the 5-minute and 1-hour windows (99% target). Pages.

**Triage.** As for [`IAMRealmProvisionerKeycloakCallFailureRate`](#iamrealmprovisionerkeycloakcallfailurerate).

### IAMSLO2_RealmProvisionerKeycloakSlowBurn

**Meaning.** The 1-hour Keycloak failure ratio has been above 6% for 15 minutes. Ticket. Same triage; often a single shard or a single `op`.

### IAMSLO3_RealmProvisionerReconcilerMarkersElevated

**Meaning.** `iam:realm_provisioner_reconciler_markers:sum` (pending realm-config rows + the last hour's trial-expiry deferrals) has stayed above 5 for 20 minutes. A reconciler is not converging.

**Triage.** Check which term is high, then follow [`IAMRealmProvisionerRealmConfigPendingSustained`](#iamrealmprovisionerrealmconfigpendingsustained) or [`IAMRealmProvisionerTrialExpiryDeferredSustained`](#iamrealmprovisionertrialexpirydeferredsustained). Confirm the CronJobs ran (`kubectl get jobs -l app.kubernetes.io/component` / `cronjob-*`) and that their metrics reach the Pushgateway.

### IAMRealmProvisionerOpenBaoErrors

**Meaning.** OpenBao KV or Kubernetes-auth operations returned errors (`iam_realm_provisioner_openbao_errors_total{op}`). RP-6/RP-7 (IdP configure/remove) will fail completely when OpenBao is unreachable.

**Triage.** Check (1) pod ServiceAccount token validity (`kubectl exec ... -- cat /var/run/secrets/kubernetes.io/serviceaccount/token | jwt decode`), (2) OpenBao health (`BAO_ADDR` reachability from the pod), (3) OpenBao Kubernetes auth role (`BAO_AUTH_ROLE`) is correctly configured, (4) op label in the metric to identify whether this is an auth failure (`kubernetes_login`) or a KV operation failure.

### IAMRealmProvisionerTrialHardDeleteFailure

**Meaning.** The RP-C2 `trial-hard-delete` CronJob is failing to GDPR hard-delete one or more trial tenants (`iam_realm_provisioner_trial_hard_delete_total{result="failed"}`). PII erasure is not completing on schedule.

**Triage.** Check the `trial-hard-delete` CronJob logs (`kubectl logs -l cronjob=trial-hard-delete`). Common causes: Keycloak unreachable when calling `HardDeleteUsersInRealm`; `SYSTEM_DATABASE_URL` not set (the GDPR scrub requires a BYPASSRLS role); the tenant is already in an unexpected state. The job is idempotent — the failed tenant remains `status=disabled` and will be retried on the next CronJob run.

### IAMRealmProvisionerPruneJobsStale

**Meaning.** A prune job (`outbox`, `processed_events`, or `ledger`) has not deleted any rows in the last 26 hours (`iam_realm_provisioner_prune_rows_total`). One or more tables may be growing unbounded.

**Triage.** Check (1) whether the CronJob ran (`kubectl get jobs -l cronjob=<job>-prune`), (2) the Pushgateway has the metric (`curl <PROMETHEUS_PUSHGATEWAY_URL>/metrics | grep prune_rows`), (3) whether the table actually has old rows past the retention window (`SELECT COUNT(*) FROM outbox_events WHERE ...`). An empty table is not a problem — this alert only fires when the job ran but deleted nothing, suggesting either a retention-window misconfiguration or no prunable rows (benign in low-traffic environments).
