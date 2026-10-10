// Package metrics registers this service's Prometheus instruments under
// the Enterprise Platform Observability Standard's three-tier hierarchy
// (§11.2, §25):
//
//   - Tier 1 platform_* — owned exclusively by platform-events v2 and
//     platform-gincommon. platform-events registers platform_messages_*,
//     platform_retry_total, platform_dlq_messages_total,
//     platform_duplicate_messages_total, platform_dependency_request_seconds,
//     platform_event_propagation_seconds, platform_build_info, and the full
//     outbox/queue-depth family when events.InitMetrics is called in main.go.
//     This service MUST NOT register any platform_* collector independently:
//     a duplicate registration with the wrong label schema causes a
//     RegistrationWarning that silences the library's own copy (fail-soft),
//     breaking cross-service aggregation. The Platform Observability Registry
//     in platform-gincommon's obsregistry/registry.json is authoritative.
//
//   - Tier 2 iam_* (domain-shared) — none currently apply. RP-INV-1 (this
//     service is the sole Keycloak Admin API caller in the platform) means
//     every remaining metric represents a behavior no other IAM service can
//     perform, failing the standard's "shared across IAM services" test.
//
//   - Tier 3 iam_realm_provisioner_* (§25 — the frozen prefix) — this
//     service's authoritative, alertable metrics. All alerts, SLO recording
//     rules, and dashboards in this repo read these names.
//
// All collectors are registered on gincommon.MetricsRegisterer() — never on
// the default Prometheus global registerer — so they share the same registry
// as gincommon's own HTTP metrics and carry the {domain, service, environment}
// const labels injected by ObservabilityMiddlewares (gincommon.Config.Domain
// and .Environment must be set in both composition roots before Register is
// called).
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
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

	// KeycloakCallDuration is Keycloak Admin API call latency by operation.
	// Authoritative source for every Keycloak latency alert and SLO in this
	// repo. platform_dependency_request_seconds (owned by platform-events for
	// SNS/SQS/codec calls) covers a different dependency scope and is not
	// registered here to avoid label-schema conflicts.
	KeycloakCallDuration *prometheus.HistogramVec

	// KeycloakCallFailuresTotal counts Keycloak Admin API failures by operation.
	// Authoritative — every Keycloak failure-rate alert reads this counter.
	KeycloakCallFailuresTotal *prometheus.CounterVec

	// AdminTokenRefreshTotal counts admin token refreshes by trigger.
	AdminTokenRefreshTotal *prometheus.CounterVec

	// IdpValidationTotal counts §11.6 IdP validation runs by outcome.
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
	// of pending_audit_entries rows (TenantIdpConfigChanged, §10.6) by
	// result (delivered|retry|rejected).
	AuditEntryDeliveryTotal *prometheus.CounterVec

	// AuditEntryBacklogGauge is the current pending_audit_entries row
	// count by status (pending|failed).
	AuditEntryBacklogGauge *prometheus.GaugeVec

	// ServiceAccountRegisterTotal counts the RP-1/RP-2/RP-3/RP-4
	// automation-principal mint-tail attempts (mint Keycloak client +
	// Token Service TS-4/TS-1) by result.
	ServiceAccountRegisterTotal *prometheus.CounterVec

	// ServiceAccountKeyRefreshTotal counts RP-17 calls by result.
	ServiceAccountKeyRefreshTotal *prometheus.CounterVec

	// ServiceAccountBackfillTotal counts RP-C5 backfill-reconciler
	// per-tenant attempts by result (§8.14 Part 4, RP-D19).
	ServiceAccountBackfillTotal *prometheus.CounterVec

	// TemplateDrift is the report-only count of dedicated realms whose
	// realm_template_version differs from REALM_TEMPLATE_VERSION.
	TemplateDrift *prometheus.GaugeVec

	// RLSViolationsTotal counts RLS-6 violations scraped from
	// rls_violation_log, by violation_type. Authoritative — every RLS-6
	// alert in this repo reads this metric.
	RLSViolationsTotal *prometheus.CounterVec

	// UnknownEventAcknowledged counts inbound SQS events with a type this
	// service does not handle (forward-compat — silently ack, log, and
	// record processed_events). A sustained nonzero rate means a producer
	// added a type we should be handling.
	UnknownEventAcknowledged *prometheus.CounterVec

	// ConsumedSchemaViolationsTotal counts inbound SQS deliveries whose
	// payload failed this service's consumed-message schema
	// (internal/eventschema.ConsumedByConsumer, cmd/server/inbound_schema.go)
	// and were rejected before the handler. Any nonzero rate means a
	// producer broke its contract.
	ConsumedSchemaViolationsTotal *prometheus.CounterVec

	// ProcessedEventsDuplicates counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4). Authoritative Tier 3 signal
	// for deduplication rate; platform-events' platform_duplicate_messages_total
	// covers the same concept at the library level with queue+event_type labels.
	ProcessedEventsDuplicates *prometheus.CounterVec
)

// gincommonLabels returns a copy of gincommon's {domain, service, environment}
// const labels so business collectors share the same label set as HTTP metrics.
// Returns nil when ObservabilityMiddlewares has not run yet (safe zero value
// for prometheus.CounterOpts.ConstLabels).
func gincommonLabels() prometheus.Labels {
	labels := gincommon.MetricsConstLabels()
	if len(labels) == 0 {
		return nil
	}
	return labels
}

var registerOnce sync.Once

// Register wires all Tier 3 business metrics onto gincommon's Prometheus
// registerer. Call once at startup AFTER ObservabilityMiddlewares has run
// (so gincommonLabels returns the full {domain, service, environment} set)
// and BEFORE the /metrics endpoint is served. Idempotent.
func Register(environment string) {
	registerOnce.Do(func() { registerMetrics(environment) })
}

func registerMetrics(_ string) {
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
		Help:        "§11.6 IdP validation runs by outcome (pass|warnings_only|failing).",
		ConstLabels: labels,
	}, []string{"outcome"})

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
		Help:        "AL-5 direct-write audit-entry delivery attempts by result (delivered|retry|rejected).",
		ConstLabels: labels,
	}, []string{"result"})

	AuditEntryBacklogGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name:        prefix + "audit_entry_backlog_gauge",
		Help:        "Current pending_audit_entries rows by status (pending|failed).",
		ConstLabels: labels,
	}, []string{"status"})

	ServiceAccountRegisterTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "service_account_register_total",
		Help:        "RP-1/RP-2/RP-3/RP-4 automation-principal mint-tail attempts by result (success|failure).",
		ConstLabels: labels,
	}, []string{"result"})

	ServiceAccountKeyRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "service_account_key_refresh_total",
		Help:        "RP-17 service-account key-cache refresh calls by result.",
		ConstLabels: labels,
	}, []string{"result"})

	ServiceAccountBackfillTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        prefix + "service_account_backfill_total",
		Help:        "RP-C5 automation-principal backfill reconciler per-tenant attempts by result.",
		ConstLabels: labels,
	}, []string{"result"})

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
		Help:        "Inbound deliveries rejected before the handler because the payload failed its consumed-message schema (retried until SQS redrives to -dlq), by consumer and event type — any nonzero rate means a producer contract break.",
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
		LapseProcessedTotal,
		RealmExportTotal,
		UserDeleteTotal,
		SessionRevokeTotal,
		MfaResetTotal,
		AuditEntryDeliveryTotal,
		AuditEntryBacklogGauge,
		ServiceAccountRegisterTotal,
		ServiceAccountKeyRefreshTotal,
		ServiceAccountBackfillTotal,
		TemplateDrift,
		RLSViolationsTotal,
		UnknownEventAcknowledged,
		ConsumedSchemaViolationsTotal,
		ProcessedEventsDuplicates,
	)

	// Pre-initialize labels so dashboards show 0 rather than "no data".
	RLSViolationsTotal.WithLabelValues("missing_or_invalid_guc")
	RLSViolationsTotal.WithLabelValues("cross_tenant_access")
}
