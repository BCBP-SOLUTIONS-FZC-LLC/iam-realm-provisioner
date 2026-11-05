// Package metrics registers this service's Prometheus instruments under
// the Enterprise Platform Observability Standard's three-tier hierarchy
// (§11.2, §25). The Platform Observability Registry (platform-gincommon's
// pkg/obsregistry) is the single source of truth for every shared name:
//
//   - Tier 1 platform_* — shared across every domain, labeled
//     {domain, service, environment}. This service emits exactly one Tier 1
//     metric itself: DependencyRequestDuration
//     (platform_dependency_request_seconds{dependency="keycloak"}), built
//     from its registry entry (gincommon.NewSharedHistogramVec) and sharing
//     the one collector platform-events also records SNS/SQS/codec calls
//     on. Its registry status is "proposed" (awaiting governance), so
//     every rule that reads it carries metric_status: proposed. Every other
//     platform_* series this process exposes (HTTP, messaging, outbox, DB)
//     is emitted by platform-gincommon, platform-events and
//     platform-pgcommon themselves.
//   - Tier 2 iam_* (domain-shared) — none currently apply. RP-INV-1 (this
//     service is the sole Keycloak Admin API caller in the platform)
//     means every remaining metric represents a behavior no other IAM
//     service can perform, failing the standard's "shared across IAM
//     services" test.
//   - Tier 3 iam_realm_provisioner_* (§25 — the frozen prefix, which is
//     also the registry's <domain>_<service>_ prefix for ServiceName
//     "iam-realm-provisioner") — everything else.
//
// Every collector carries gincommon's {domain, service, environment} const
// labels (gincommon.MetricsConstLabels) and registers on
// gincommon.MetricsRegisterer, so one /metrics scrape serves all of them.
package metrics

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

const prefix = "iam_realm_provisioner_" // Tier 3 — frozen (§25).

var (
	// ProvisionTotal counts provisioning attempts by kind and result.
	ProvisionTotal *prometheus.CounterVec

	// UsersMigratedTotal is the cumulative count of users migrated
	// trial→dedicated at conversion.
	UsersMigratedTotal prometheus.Counter

	// RealmActiveGauge is the current count of active realms by strategy.
	RealmActiveGauge *prometheus.GaugeVec

	// KeycloakCallDuration is Keycloak Admin API call latency by operation
	// (Tier 3, op values frozen by §25). DependencyRequestDuration records
	// the same calls under the shared Tier 1 name.
	KeycloakCallDuration *prometheus.HistogramVec

	// KeycloakCallFailuresTotal counts Keycloak Admin API failures by
	// operation (Tier 3). The Tier 1 equivalent is
	// platform_dependency_request_seconds_count{dependency="keycloak",outcome="error"}.
	KeycloakCallFailuresTotal *prometheus.CounterVec

	// AdminTokenRefreshTotal counts admin token refreshes by trigger.
	AdminTokenRefreshTotal *prometheus.CounterVec

	// IdpValidationTotal counts §11.6 IdP validation runs by result
	// (pass|warnings_only|failing).
	IdpValidationTotal *prometheus.CounterVec

	// MfaRoleSyncTotal counts requires-mfa realm-role syncs by action.
	MfaRoleSyncTotal *prometheus.CounterVec

	// RealmConfigPendingGauge is the current count of pending
	// realm_config_reconciliations rows.
	RealmConfigPendingGauge prometheus.Gauge

	// TrialExpiryProcessedTotal counts RP-C1 trial-expiry rows processed.
	TrialExpiryProcessedTotal prometheus.Counter

	// TrialExpiryDeferredTotal counts RP-C1 trial-expiry rows deferred.
	TrialExpiryDeferredTotal prometheus.Counter

	// OrphanedSignupReapTotal counts orphaned-signup-reap per-tenant
	// outcomes by result (reaped|skipped|deferred).
	OrphanedSignupReapTotal *prometheus.CounterVec

	// LapseProcessedTotal counts RP-C3 lapse-sweep transitions by kind.
	LapseProcessedTotal *prometheus.CounterVec

	// RealmExportTotal counts realm export-to-S3 attempts by result.
	RealmExportTotal *prometheus.CounterVec

	// UserDeleteTotal counts RP-15 user-delete calls by result.
	UserDeleteTotal *prometheus.CounterVec

	// SessionRevokeTotal counts RP-16 session-revoke calls by result.
	SessionRevokeTotal *prometheus.CounterVec

	// MfaResetTotal counts RP-9 MFA-reset calls by result.
	MfaResetTotal *prometheus.CounterVec

	// AuditEntryDeliveryTotal counts AL-5 direct-write delivery attempts
	// of pending_audit_entries rows (TenantIdpConfigChanged, TermsAccepted;
	// §10.6) by result (delivered|retry|unknown_entry_type|rejected).
	// unknown_entry_type is Audit Log not knowing the type yet (deploy
	// skew): the row stays pending and is retried, never parked.
	AuditEntryDeliveryTotal *prometheus.CounterVec

	// AuditEntryBacklogGauge is the current pending_audit_entries row
	// count by status (pending|failed) — a growing pending count means
	// Audit Log is unreachable or AUDIT_LOG_BASE_URL is unset; any failed
	// row is an entry Audit permanently rejected.
	AuditEntryBacklogGauge *prometheus.GaugeVec

	// ServiceAccountRegisterTotal counts the RP-1/RP-2/RP-3
	// automation-principal mint-tail attempts (mint Keycloak client +
	// Token Service TS-4/TS-1) by result — "warn" is the non-blocking
	// Token-Service-unreachable-at-mint-time outcome, distinct from
	// "fail" (§8.14 Part 1, planned RP-D17).
	ServiceAccountRegisterTotal *prometheus.CounterVec

	// ServiceAccountKeyRefreshTotal counts RP-17 calls by result.
	ServiceAccountKeyRefreshTotal *prometheus.CounterVec

	// OpenBaoErrorsTotal counts OpenBao KV/auth operation errors by op
	// (write_secret|read_secret|delete_secret|kubernetes_login) and result.
	// C2 — no silent failure path for vault operations.
	OpenBaoErrorsTotal *prometheus.CounterVec

	// TrialHardDeleteTotal counts RP-C2 trial-hard-delete per-tenant
	// outcomes by result (succeeded|failed). M2 — GDPR erasure observability.
	TrialHardDeleteTotal *prometheus.CounterVec

	// PruneRowsTotal counts rows deleted by each prune job by job
	// (outbox|processed_events|ledger) and table. M3 — table growth alertability.
	PruneRowsTotal *prometheus.CounterVec

	// IdempotencyReplayTotal counts provisioning requests served from the
	// idempotency ledger replay (not fresh execution), by kind. L1.
	IdempotencyReplayTotal *prometheus.CounterVec

	// CompensationTotal counts compensating teardown outcomes by op
	// (delete_realm|delete_service_account_client) and result
	// (success|failure). L2 — systematic cleanup failures visible in dashboards.
	CompensationTotal *prometheus.CounterVec

	// TemplateDrift is the report-only count of dedicated realms whose
	// realm_template_version differs from REALM_TEMPLATE_VERSION.
	TemplateDrift *prometheus.GaugeVec

	// RLSViolationsTotal counts RLS-6 violations scraped from
	// rls_violation_log, by violation_type. Tier 3 and the only RLS metric:
	// the Platform Observability Registry has no platform-wide RLS metric,
	// so this service no longer emits a platform_* name for it.
	RLSViolationsTotal *prometheus.CounterVec

	// UnknownEventAcknowledged counts inbound SQS events with a type this
	// service does not handle (forward-compat — silently ack, log, and
	// record processed_events, matching iam-org-membership). A sustained
	// nonzero rate means a producer added a type we should be handling.
	//
	// Not promoted: this is the Platform Observability Standard's own
	// worked Tier 3 example (iam_event_consumer_unknown_event_acknowledged_total),
	// so it stays service-specific by the standard's own classification.
	UnknownEventAcknowledged *prometheus.CounterVec

	// ConsumedSchemaViolationsTotal counts inbound SQS deliveries whose
	// payload failed this service's consumed-message schema
	// (internal/eventschema.ConsumedByConsumer, cmd/server/inbound_schema.go)
	// and were rejected before the handler and processed_events — once per
	// handler delivery, so one bad message counts up to 4 times
	// (dlqHandler takes the 5th) before SQS redrives it to -dlq. Any nonzero rate means a producer
	// broke its contract (or api/asyncapi.yaml is stale) and every event of
	// that type is being dropped — pages (deploy/monitoring/app-alerts.yml).
	// Tier 3: the authoritative, alertable signal; labels are bounded by
	// the 3 consumers x their 6 consumed types.
	ConsumedSchemaViolationsTotal *prometheus.CounterVec

	// ProcessedEventsDuplicates counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4), by consumer (Tier 3).
	// platform-events' inbox (which this service dedups through) records
	// the registry's platform_duplicate_messages_total{queue,event_type}
	// itself; this Tier 3 counter stays the authoritative alerting source
	// while that registry entry is proposed.
	ProcessedEventsDuplicates *prometheus.CounterVec

	// DependencyRequestDuration is the Tier 1
	// platform_dependency_request_seconds histogram (registry status
	// "proposed"): one sample per outbound Keycloak Admin API call, labeled
	// dependency="keycloak", operation (the frozen §25 op names, e.g.
	// create_realm, token) and outcome (success|error). It is the same
	// collector platform-events records its SNS/SQS/codec calls on: Register
	// builds it from the registry entry before events.InitMetrics runs, and
	// platform-events reuses an identical registered collector.
	DependencyRequestDuration *prometheus.HistogramVec
)

// gincommonLabels returns a copy of gincommon's {domain, service,
// environment} const labels so business collectors scrape on the same
// registry and identity as gincommon's HTTP metrics. Returns nil when
// ObservabilityMiddlewares has not run yet, matching prometheus's "no const
// labels" zero value.
func gincommonLabels() prometheus.Labels {
	labels := gincommon.MetricsConstLabels()
	if len(labels) == 0 {
		return nil
	}
	return labels
}

var registerOnce sync.Once

// Register wires business metrics onto gincommon's Prometheus registerer
// (same registry and {domain, service, environment} const labels as HTTP
// metrics). Call once at startup AFTER ObservabilityMiddlewares has run
// (the identity is not known before) and BEFORE events.InitMetrics, so
// platform-events reuses DependencyRequestDuration. Idempotent. It panics
// if a collector cannot be registered, like prometheus.MustRegister.
func Register() {
	registerOnce.Do(registerMetrics)
}

// sharedHistogram builds the registry histogram name on gincommon's
// registerer, reusing an identical collector another library (platform-
// events) registered first.
func sharedHistogram(name string) *prometheus.HistogramVec {
	h, err := gincommon.NewSharedHistogramVec(nil, name)
	if err == nil {
		return h
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if existing, ok := are.ExistingCollector.(*prometheus.HistogramVec); ok {
			return existing
		}
	}
	panic("metrics: register " + name + ": " + err.Error())
}

func registerMetrics() {
	labels := gincommonLabels()

	ProvisionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "provision_total",
		Help:        "Provisioning attempts by kind (trial|direct|convert) and result.",
		ConstLabels: labels,
	}, []string{"kind", "result"})

	UsersMigratedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prefix + "users_migrated_total",
		Help:        "Cumulative count of users migrated trial->dedicated at conversion.",
		ConstLabels: labels,
	})

	RealmActiveGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        prefix + "realm_active_gauge",
		Help:        "Current count of active realms by strategy (shared|dedicated).",
		ConstLabels: labels,
	}, []string{"strategy"})

	KeycloakCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        prefix + "keycloak_call_duration_seconds",
		Help:        "Keycloak Admin API call latency by operation.",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: labels,
	}, []string{"op"})

	KeycloakCallFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "keycloak_call_failures_total",
		Help:        "Keycloak Admin API call failures by operation.",
		ConstLabels: labels,
	}, []string{"op"})

	AdminTokenRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "admin_token_refresh_total",
		Help:        "Keycloak admin token refreshes by trigger (proactive|401).",
		ConstLabels: labels,
	}, []string{"trigger"})

	IdpValidationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "idp_validation_total",
		Help:        "§11.6 IdP validation runs by result (pass|warnings_only|failing).",
		ConstLabels: labels,
		// "result", not "outcome": the registry reserves outcome for
		// success|failure|error on every metric.
	}, []string{"result"})

	MfaRoleSyncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "mfa_role_sync_total",
		Help:        "requires-mfa realm-role sync operations by action (assign|revoke).",
		ConstLabels: labels,
	}, []string{"action"})

	RealmConfigPendingGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        prefix + "realm_config_pending_gauge",
		Help:        "Current count of realm_config_reconciliations rows in status=pending.",
		ConstLabels: labels,
	})

	TrialExpiryProcessedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prefix + "trial_expiry_processed_total",
		Help:        "RP-C1 trial-expiry rows successfully processed.",
		ConstLabels: labels,
	})

	TrialExpiryDeferredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        prefix + "trial_expiry_deferred_total",
		Help:        "RP-C1 trial-expiry rows deferred to the next tick.",
		ConstLabels: labels,
	})

	OrphanedSignupReapTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "orphaned_signup_reap_total",
		Help:        "orphaned-signup-reap per-tenant outcomes: signups that never received TrialStarted, by result (reaped|skipped|deferred).",
		ConstLabels: labels,
	}, []string{"result"})

	LapseProcessedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "lapse_processed_total",
		Help:        "RP-C3 lapse-sweep transitions by kind (suspend|offboard).",
		ConstLabels: labels,
	}, []string{"transition"})

	RealmExportTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "realm_export_total",
		Help:        "Realm export-to-S3 attempts at offboarding by result.",
		ConstLabels: labels,
	}, []string{"result"})

	UserDeleteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "user_delete_total",
		Help:        "RP-15 user-delete calls by result.",
		ConstLabels: labels,
	}, []string{"result"})

	SessionRevokeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "session_revoke_total",
		Help:        "RP-16 session-revoke calls by result.",
		ConstLabels: labels,
	}, []string{"result"})

	MfaResetTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "mfa_reset_total",
		Help:        "RP-9 MFA-reset calls by result.",
		ConstLabels: labels,
	}, []string{"result"})

	AuditEntryDeliveryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "audit_entry_delivery_total",
		Help:        "AL-5 direct-write audit-entry delivery attempts by result (delivered|retry|unknown_entry_type|rejected).",
		ConstLabels: labels,
	}, []string{"result"})

	AuditEntryBacklogGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        prefix + "audit_entry_backlog_gauge",
		Help:        "Current pending_audit_entries rows by status (pending|failed).",
		ConstLabels: labels,
	}, []string{"status"})

	ServiceAccountRegisterTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "service_account_register_total",
		Help:        "RP-1/RP-2/RP-3 automation-principal mint-tail attempts by result (success|warn|failure).",
		ConstLabels: labels,
	}, []string{"result"})

	ServiceAccountKeyRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "service_account_key_refresh_total",
		Help:        "RP-17 service-account key-cache refresh calls by result.",
		ConstLabels: labels,
	}, []string{"result"})

	OpenBaoErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "openbao_errors_total",
		Help:        "OpenBao KV/auth operation errors by op (write_secret|read_secret|delete_secret|kubernetes_login) and result.",
		ConstLabels: labels,
	}, []string{"op", "result"})

	TrialHardDeleteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "trial_hard_delete_total",
		Help:        "RP-C2 trial-hard-delete per-tenant outcomes by result (succeeded|failed).",
		ConstLabels: labels,
	}, []string{"result"})

	PruneRowsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "prune_rows_total",
		Help:        "Rows deleted by prune jobs by job (outbox|processed_events|ledger) and table.",
		ConstLabels: labels,
	}, []string{"job", "table"})

	IdempotencyReplayTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "idempotency_replay_total",
		Help:        "Provisioning requests served from idempotency ledger replay (not fresh execution), by kind.",
		ConstLabels: labels,
	}, []string{"kind"})

	CompensationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "compensation_total",
		Help:        "Compensating teardown outcomes by op (delete_realm|delete_service_account_client) and result (success|failure).",
		ConstLabels: labels,
	}, []string{"op", "result"})

	TemplateDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        prefix + "template_drift",
		Help:        "Report-only count of dedicated realms whose realm_template_version differs from the current REALM_TEMPLATE_VERSION, by realm_version.",
		ConstLabels: labels,
	}, []string{"realm_version"})

	RLSViolationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "rls_violations_total",
		Help:        "Row-level-security violations scraped from rls_violation_log (RLS-6, sampled 1% at the source), by violation_type.",
		ConstLabels: labels,
	}, []string{"violation_type"})

	UnknownEventAcknowledged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "unknown_event_acknowledged_total",
		Help:        "Inbound events silently acknowledged because no handler is wired for the type — sustained nonzero rate means a producer added a new type.",
		ConstLabels: labels,
	}, []string{"topic", "event_type"})

	ConsumedSchemaViolationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "consumed_schema_violations_total",
		Help:        "Inbound deliveries rejected before the handler because the payload failed its consumed-message schema (forwarded straight to -dlq on first delivery), by consumer and event type — any nonzero rate means a producer contract break.",
		ConstLabels: labels,
	}, []string{"consumer", "event_type"})

	ProcessedEventsDuplicates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "processed_events_duplicates_total",
		Help:        "SQS redeliveries filtered by processed_events composite PK (IDEMP-4).",
		ConstLabels: labels,
	}, []string{"consumer"})

	gincommon.MetricsRegisterer().MustRegister(
		ProvisionTotal,
		UsersMigratedTotal,
		RealmActiveGauge,
		KeycloakCallDuration,
		KeycloakCallFailuresTotal,
		AdminTokenRefreshTotal,
		IdpValidationTotal,
		MfaRoleSyncTotal,
		RealmConfigPendingGauge,
		TrialExpiryProcessedTotal,
		TrialExpiryDeferredTotal,
		OrphanedSignupReapTotal,
		LapseProcessedTotal,
		RealmExportTotal,
		UserDeleteTotal,
		SessionRevokeTotal,
		MfaResetTotal,
		AuditEntryDeliveryTotal,
		AuditEntryBacklogGauge,
		ServiceAccountRegisterTotal,
		ServiceAccountKeyRefreshTotal,
		OpenBaoErrorsTotal,
		TrialHardDeleteTotal,
		PruneRowsTotal,
		IdempotencyReplayTotal,
		CompensationTotal,
		TemplateDrift,
		RLSViolationsTotal,
		UnknownEventAcknowledged,
		ConsumedSchemaViolationsTotal,
		ProcessedEventsDuplicates,
	)

	DependencyRequestDuration = sharedHistogram("platform_dependency_request_seconds")

	// Pre-initialize labels so dashboards show 0 rather than "no data".
	RLSViolationsTotal.WithLabelValues("missing_or_invalid_guc")
	RLSViolationsTotal.WithLabelValues("cross_tenant_access")
}
