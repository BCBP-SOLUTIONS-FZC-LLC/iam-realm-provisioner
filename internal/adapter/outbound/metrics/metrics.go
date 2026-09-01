// Package metrics registers this service's iam_realm_provisioner_*
// Prometheus instruments (§11.2, §23 — the frozen metric prefix). Business
// counters/gauges live here; HTTP/outbox/SQS metrics come free from
// platform-gincommon and platform-events.
package metrics

import "github.com/prometheus/client_golang/prometheus"

const prefix = "iam_realm_provisioner_"

// The service's iam_realm_provisioner_* business instruments (§11.2).
// Register adds them to the default registry.
var (
	ProvisionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "provision_total",
		Help: "Provisioning attempts by kind (trial|direct|convert) and result.",
	}, []string{"kind", "result"})

	UsersMigratedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: prefix + "users_migrated_total",
		Help: "Cumulative count of users migrated trial->dedicated at conversion.",
	})

	RealmActiveGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "realm_active_gauge",
		Help: "Current count of active realms by strategy (shared|dedicated).",
	}, []string{"strategy"})

	KeycloakCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    prefix + "keycloak_call_duration_seconds",
		Help:    "Keycloak Admin API call latency by operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"op"})

	KeycloakCallFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "keycloak_call_failures_total",
		Help: "Keycloak Admin API call failures by operation.",
	}, []string{"op"})

	AdminTokenRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "admin_token_refresh_total",
		Help: "Keycloak admin token refreshes by trigger (proactive|401).",
	}, []string{"trigger"})

	IdpValidationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "idp_validation_total",
		Help: "§11.6 IdP validation runs by outcome (pass|warnings_only|failing).",
	}, []string{"outcome"})

	MfaRoleSyncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "mfa_role_sync_total",
		Help: "requires-mfa realm-role sync operations by action (assign|revoke).",
	}, []string{"action"})

	RealmConfigPendingGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: prefix + "realm_config_pending_gauge",
		Help: "Current count of realm_config_reconciliations rows in status=pending.",
	})

	TrialExpiryProcessedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: prefix + "trial_expiry_processed_total",
		Help: "RP-C1 trial-expiry rows successfully processed.",
	})

	TrialExpiryDeferredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: prefix + "trial_expiry_deferred_total",
		Help: "RP-C1 trial-expiry rows deferred to the next tick.",
	})

	LapseProcessedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "lapse_processed_total",
		Help: "RP-C3 lapse-sweep transitions by kind (suspend|offboard).",
	}, []string{"transition"})

	RealmExportTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "realm_export_total",
		Help: "Realm export-to-S3 attempts at offboarding by result.",
	}, []string{"result"})

	UserDeleteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "user_delete_total",
		Help: "RP-15 user-delete calls by result.",
	}, []string{"result"})

	SessionRevokeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "session_revoke_total",
		Help: "RP-16 session-revoke calls by result.",
	}, []string{"result"})

	MfaResetTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "mfa_reset_total",
		Help: "RP-9 MFA-reset calls by result.",
	}, []string{"result"})

	TemplateDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "template_drift",
		Help: "Report-only count of dedicated realms whose realm_template_version differs from the current REALM_TEMPLATE_VERSION, by realm_version.",
	}, []string{"realm_version"})
)

// Register adds every instrument above to the default Prometheus registry.
// Idempotent-safe to call once at process startup.
func Register() {
	prometheus.MustRegister(
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
	)
}
