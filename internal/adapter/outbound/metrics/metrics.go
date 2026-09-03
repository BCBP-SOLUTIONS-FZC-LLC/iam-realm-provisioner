// Package metrics registers this service's iam_realm_provisioner_*
// Prometheus instruments (§11.2, §25 — the frozen metric prefix). Business
// counters/gauges live here; HTTP/outbox/SQS metrics come free from
// platform-gincommon and platform-events.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

const prefix = "iam_realm_provisioner_"

var (
	// ProvisionTotal counts provisioning attempts by kind and result.
	ProvisionTotal *prometheus.CounterVec

	// UsersMigratedTotal is the cumulative count of users migrated
	// trial→dedicated at conversion.
	UsersMigratedTotal prometheus.Counter

	// RealmActiveGauge is the current count of active realms by strategy.
	RealmActiveGauge *prometheus.GaugeVec

	// KeycloakCallDuration is Keycloak Admin API call latency by operation.
	KeycloakCallDuration *prometheus.HistogramVec

	// KeycloakCallFailuresTotal counts Keycloak Admin API failures by operation.
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

	// TemplateDrift is the report-only count of dedicated realms whose
	// realm_template_version differs from REALM_TEMPLATE_VERSION.
	TemplateDrift *prometheus.GaugeVec

	// RLSViolationsTotal counts RLS-6 violations scraped from
	// rls_violation_log, by violation_type.
	RLSViolationsTotal *prometheus.CounterVec

	// UnknownEventAcknowledged counts inbound SQS events with a type this
	// service does not handle (forward-compat — silently ack, log, and
	// record processed_events, matching iam-org-membership). A sustained
	// nonzero rate means a producer added a type we should be handling.
	UnknownEventAcknowledged *prometheus.CounterVec

	// ProcessedEventsDuplicates counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4).
	ProcessedEventsDuplicates *prometheus.CounterVec
)

// gincommonLabels returns a copy of gincommon's {service, version} const
// labels so business collectors scrape on the same registry and labels as
// HTTP metrics. Returns nil when ObservabilityMiddlewares has not run yet,
// matching prometheus's "no const labels" zero value so unit tests that
// never bootstrap gincommon still register cleanly.
func gincommonLabels() prometheus.Labels {
	labels := gincommon.MetricsConstLabels()
	if len(labels) == 0 {
		return nil
	}
	return labels
}

var registerOnce sync.Once

// Register wires business metrics onto gincommon's Prometheus registerer
// (same registry and {service, version} const labels as HTTP metrics).
// Call once at startup AFTER ObservabilityMiddlewares has run and BEFORE
// the /metrics endpoint is served. Idempotent.
func Register() {
	registerOnce.Do(registerMetrics)
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
		TemplateDrift,
		RLSViolationsTotal,
		UnknownEventAcknowledged,
		ProcessedEventsDuplicates,
	)

	// Pre-initialize labels so dashboards show 0 rather than "no data".
	RLSViolationsTotal.WithLabelValues("missing_or_invalid_guc")
	RLSViolationsTotal.WithLabelValues("cross_tenant_access")
}
