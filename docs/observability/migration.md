# Renamed and removed metric names

iam-realm-provisioner moved to the Enterprise Platform Observability Standard in three steps. The service has never been deployed to any environment, so no dashboard, alert or stored series depended on the old names. Every old name was **removed outright, with no compatibility period**, and the alerts, recording rules, SLOs, HPA rule and prometheus-adapter rule in this repo were rewritten in the same change. `TestMonitoring_NoRemovedMetrics` (`metrics-registry-lint.sh`) and `metricslint refs` (`metricslint.sh`) reject any removed name that comes back.

Unlike iam-org-membership, this service's Tier 3 names never moved: the `iam_realm_provisioner_` prefix was frozen at v1.0 sign-off (LLD §25) and is also the standard's `<domain>_<service>_` prefix, so step 1 only added names.

## 1. Three-tier hierarchy (LLD rev 1.7–1.8, 2026-09-11)

The service began shadow-emitting `platform_*` Tier 1 candidates it built itself, next to the unchanged Tier 3 metrics. Rev 1.8 reverted rev 1.7's alert cutover: a service may not adopt a `platform_*` name before registry ratification, so alerts and SLOs stayed on Tier 3.

| Service-built name (rev 1.7) | Tier 3 counterpart (unchanged) | Fate in step 2 |
|---|---|---|
| `platform_dependency_request_seconds{dependency,operation,outcome}` | `iam_realm_provisioner_keycloak_call_duration_seconds`, `_keycloak_call_failures_total` | **kept**, now built from the registry entry (`gincommon.NewSharedHistogramVec`) and shared with platform-events |
| `platform_duplicate_messages_total{consumer}` | `iam_realm_provisioner_processed_events_duplicates_total` | removed from the service — platform-events' inbox records it (`{queue,event_type}`) |
| `platform_rls_violations_total{violation_type}` | `iam_realm_provisioner_rls_violations_total` | removed — never in the registry; the IAM concept is Tier 2 `iam_rls_violations_total` (see [README](README.md#items-awaiting-ratification)) |
| `platform_messages_received_total{consumer}`, `_processed_total`, `_failed_total`, `platform_dlq_messages_total{consumer}` | — | removed — platform-events records them itself, labelled by `queue` and `event_type` |

## 2. Platform library migration (2026-10-02)

The platform libraries dropped their pre-standard names (platform-gincommon v1.4.0, platform-events/v2, platform-pgcommon/v2). Every query in this repo was rewritten to the `platform_*` successor:

| Removed name | Replacement | Label changes |
|---|---|---|
| `http_requests_total` | `platform_http_requests_total` | + `domain`, `environment`; − `version` |
| `http_request_duration_seconds` | `platform_http_request_duration_seconds` | same; identical buckets |
| `http_requests_per_second` (prometheus-adapter) | `platform_http_requests_per_second` | derived from `platform_http_requests_total` |
| `outbox_dead_letters_total` | `platform_dlq_messages_total{operation="outbox_publish"}` | `reason="max_attempts"` |
| `sqs_receive_errors_total{queue}` | `platform_dependency_request_seconds_count{dependency="sqs",operation="receive_message",outcome="error"}` | no `queue` label (per service) |

The identity became mandatory at the same time: `domain="iam"` via `gincommon.Config.Domain` / `OBSERVABILITY_DOMAIN`, and `APP_ENV` restricted to `local, dev, test, staging, prod` (`production` is rejected). platform-pgcommon's `pgcommon_*` and platform-events' `events_*` / `outbox_*` / `sqs_*` names were removed the same way; their replacements are in each library's `docs/observability/`.

## 3. Library incorporation (LLD rev 1.20, 2026-10-03/04)

No metric was renamed. These changes alter where series come from or what they mean:

| Before | After | Why |
|---|---|---|
| `/metrics` served by a bare `promhttp.Handler()` | `gincommon.MetricsHandler()` | The bare handler served Prometheus's default registry, not `gincommon.MetricsRegisterer()` |
| `TimeoutMiddleware(30s)` as an outer `r.Use` | `gincommon.Config.RequestTimeout` (innermost observability handler) | Timed-out requests were recorded as 200 in `platform_http_*`; they now record the 503 the client receives, and count in `platform_http_request_timeouts_total` |
| Reconciler metrics never exported | pushed to `PROMETHEUS_PUSHGATEWAY_URL`, grouped by `cronjob` | CronJobs have no `/metrics` listener |
| Reconciler identity `service="iam-realm-provisioner-reconciler"` | `service="iam-realm-provisioner"` | `iam_realm_provisioner_*` series must carry the service whose prefix they use |
| Consumed-schema violations retried until SQS redrive, `dlqHandler` observing the 5th delivery as `platform_messages_failed_total{reason="dead_letter_error"}` | forwarded to `-dlq` on the first delivery: `platform_dlq_messages_total{operation="consume",reason="explicit"}`; persistent handler failures on the 5th delivery: `reason="max_receive_count"` | DLQ forwarding moved onto platform-events (`WithDLQForwarding`, `sqspipeline`) |
| `platform_dlq_depth` / `platform_queue_depth` not sampled | sampled every `SQS_QUEUE_DEPTH_INTERVAL` (60s default) | `sqspipeline.EnvForQueue` turns sampling on |
| `platform_duplicate_messages_total` not recorded (service-owned dedup table) | recorded by platform-events' inbox | `processed_events` dedup moved onto `pkg/inbox` |
| DB statements cancelled by `statement_timeout` (SQLSTATE 57014) reported as `db_unavailable` | no longer an outage (pgcommon's `IsOperatorIntervention` excludes 57014) | Fewer false 503s in `platform_http_requests_total{status_class="5xx"}` |

## Querying after the migration

```promql
# DLQ inflow from every cause, counted once (library)
sum by (environment, operation, reason) (increase(platform_dlq_messages_total{service="iam-realm-provisioner"}[15m]))

# Consumed-schema rejects by consumer and type (Tier 3, authoritative)
sum by (consumer, event_type) (rate(iam_realm_provisioner_consumed_schema_violations_total[5m]))

# Keycloak failure ratio (Tier 3, SLO-2 source)
sum(rate(iam_realm_provisioner_keycloak_call_failures_total[5m]))
  / sum(rate(iam_realm_provisioner_keycloak_call_duration_seconds_count[5m]))

# Reconciler runs (Pushgateway)
iam_realm_provisioner_trial_expiry_deferred_total{cronjob="trial-expiry"}
```
