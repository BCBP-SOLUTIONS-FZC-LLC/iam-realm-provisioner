// Package metrics registers this service's Prometheus instruments under
// the Enterprise Platform Observability Standard's three-tier hierarchy
// (§11.2, §25):
//
//   - Tier 1 platform_* — shared across every domain, labeled
//     {domain, service, environment}. The standard's governance rules
//     (9-12) are explicit: metric names/labels are part of a shared
//     contract, and a service MUST NOT unilaterally invent or adopt a new
//     platform_* metric into production alerting/dashboards/SLOs/HPA
//     before it clears the Platform Observability Registry's ratification
//     process. This file emits three platform_* candidates —
//     DependencyRequestDuration (platform_dependency_request_seconds),
//     DuplicateMessagesTotal (platform_duplicate_messages_total), and
//     RLSViolationsPlatformTotal (platform_rls_violations_total) — in
//     shadow, alongside their existing Tier 3 counterparts, so real
//     production data backs their ratification packets. See
//     PlatformRegistry (registry.go) for each candidate's full submitted
//     packet — status, semantic definition, required/approved labels and
//     their allowed values, cardinality justification, aggregation
//     expectations. Every alert/recording-rule/SLO in this repo continues
//     to read the Tier 3 metric until a candidate's status there flips
//     from Proposed to Canonical.
//   - Tier 2 iam_* (domain-shared) — none currently apply. RP-INV-1 (this
//     service is the sole Keycloak Admin API caller in the platform)
//     means every remaining metric represents a behavior no other IAM
//     service can perform, failing the standard's "shared across IAM
//     services" test.
//   - Tier 3 iam_realm_provisioner_* (§25 — the frozen prefix) — everything
//     else, and still this service's sole authoritative metric family for
//     every one of the three Tier 1 candidates above; nothing here is
//     renamed or scheduled for removal by those candidates existing.
//
// HTTP/outbox/SQS metrics come free from platform-gincommon and
// platform-events.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

const (
	prefix = "iam_realm_provisioner_" // Tier 3 — frozen (§25).

	// platformDomain/platformService are the Platform Observability
	// Standard's required {domain, service} const-label values for this
	// service's Tier 1 (platform_*) metrics. platformService is
	// deliberately the domain-relative slug ("realm-provisioner"), not the
	// gincommon service name ("iam-realm-provisioner"/
	// "iam-realm-provisioner-reconciler") used elsewhere in this file —
	// the standard's examples key "service" without a domain prefix
	// (domain carries that), so this must match the value the other IAM
	// services use for their own platform_* metrics.
	platformDomain  = "iam"
	platformService = "realm-provisioner"
)

var (
	// ProvisionTotal counts provisioning attempts by kind and result.
	ProvisionTotal *prometheus.CounterVec

	// UsersMigratedTotal is the cumulative count of users migrated
	// trial→dedicated at conversion.
	UsersMigratedTotal prometheus.Counter

	// RealmActiveGauge is the current count of active realms by strategy.
	RealmActiveGauge *prometheus.GaugeVec

	// KeycloakCallDuration is Keycloak Admin API call latency by operation.
	//
	// This remains the authoritative source for every alert/SLO in this
	// repo. DependencyRequestDuration (platform_dependency_request_seconds)
	// is a Tier 1 candidate covering the same data plus
	// domain/service/environment labels, shadow-emitted from the same
	// call site pending Platform Observability Registry ratification
	// (PlatformRegistry in registry.go; Status: Proposed) — it does not
	// replace this metric until that status flips to Canonical.
	KeycloakCallDuration *prometheus.HistogramVec

	// KeycloakCallFailuresTotal counts Keycloak Admin API failures by operation.
	//
	// Authoritative (see KeycloakCallDuration's doc comment) — the Tier 1
	// candidate DependencyRequestDuration's outcome="failure" samples
	// cover the same signal but remain Status: Proposed, not yet approved
	// to source alerting.
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
	//
	// Authoritative — every RLS-6 alert in this repo reads this metric.
	// RLSViolationsPlatformTotal (platform_rls_violations_total) is a
	// Tier 1 candidate this service is proposing to the Platform
	// Observability Registry (RLS is a platform-wide Postgres pattern,
	// not IAM-specific), shadow-emitted from the same exporter tick;
	// Status: Proposed in PlatformRegistry (registry.go) until ratified.
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

	// ProcessedEventsDuplicates counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4).
	//
	// Authoritative — nothing in this repo alerts on DuplicateMessagesTotal
	// (platform_duplicate_messages_total{consumer}) yet; it is a Tier 1
	// candidate named explicitly in the standard's own Registry-Proposed
	// Examples, shadow-emitted from the same call site, Status: Proposed
	// in PlatformRegistry (registry.go) pending ratification.
	ProcessedEventsDuplicates *prometheus.CounterVec

	// DependencyRequestDuration is a Tier 1 candidate
	// (platform_dependency_request_seconds) submitted to the Platform
	// Observability Registry for ratification — see PlatformRegistry
	// (registry.go, Status: Proposed) for the full packet. Outbound
	// dependency-call latency, labeled by dependency
	// (currently only "keycloak"), operation, and outcome
	// (success|failure) so dependency error rate derives from this
	// histogram's sample count rather than a separate failures counter.
	// Shadow-emitted alongside the still-authoritative KeycloakCallDuration
	// and KeycloakCallFailuresTotal — per rule 12, it does not source any
	// alert/SLO until ratified.
	DependencyRequestDuration *prometheus.HistogramVec

	// DuplicateMessagesTotal is a Tier 1 candidate
	// (platform_duplicate_messages_total) submitted for ratification —
	// see PlatformRegistry (registry.go, Status: Proposed). SQS
	// redeliveries filtered by a consumer's dedup store, by consumer.
	// Shadow-emitted alongside the still-authoritative
	// ProcessedEventsDuplicates.
	DuplicateMessagesTotal *prometheus.CounterVec

	// RLSViolationsPlatformTotal is a Tier 1 candidate
	// (platform_rls_violations_total) this service is proposing to the
	// Platform Observability Registry — not one of the standard's own
	// named examples, so its packet (PlatformRegistry in registry.go,
	// Status: Proposed) argues the case that RLS-based tenant isolation
	// is a platform-wide, not IAM-specific, pattern. RLS-6 violations
	// scraped from rls_violation_log, by violation_type. Shadow-emitted
	// alongside the still-authoritative RLSViolationsTotal.
	RLSViolationsPlatformTotal *prometheus.CounterVec
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

// platformConstLabels returns the {domain, service, environment} const
// labels the Platform Observability Standard requires on every Tier 1
// (platform_*) metric. This is deliberately independent of
// gincommonLabels()'s {service, version}: gincommon's "service" is this
// binary's process name, while the standard's "service" is the
// domain-relative slug shared with the sibling IAM services' own
// platform_*/iam_* metrics. environment is the caller-supplied APP_ENV
// value (see Register) — there is no existing shared source for it, so it
// is injected here rather than left to call sites, satisfying the
// standard's "service label MUST be injected centrally" rule for this
// label too.
func platformConstLabels(environment string) prometheus.Labels {
	return prometheus.Labels{
		"domain":      platformDomain,
		"service":     platformService,
		"environment": environment,
	}
}

var registerOnce sync.Once

// Register wires business metrics onto gincommon's Prometheus registerer
// (same registry and {service, version} const labels as HTTP metrics).
// environment is APP_ENV (e.g. dev|staging|prod), used as the Platform
// Observability Standard's "environment" const label on this service's
// Tier 1 (platform_*) metrics — pass the same value cmd/server and
// cmd/reconciler already resolve via envOr("APP_ENV", "dev") so there is
// one source of truth for it. Call once at startup AFTER
// ObservabilityMiddlewares has run and BEFORE the /metrics endpoint is
// served. Idempotent.
func Register(environment string) {
	registerOnce.Do(func() { registerMetrics(environment) })
}

func registerMetrics(environment string) {
	labels := gincommonLabels()
	platformLabels := platformConstLabels(environment)

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

	DependencyRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "platform_dependency_request_seconds",
		Help:        "Outbound dependency call latency by dependency and operation; outcome (success|failure) distinguishes failed calls so no separate failures counter is needed.",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: platformLabels,
	}, []string{"dependency", "operation", "outcome"})

	DuplicateMessagesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_duplicate_messages_total",
		Help:        "SQS redeliveries filtered by a consumer's dedup store, by consumer.",
		ConstLabels: platformLabels,
	}, []string{"consumer"})

	RLSViolationsPlatformTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_rls_violations_total",
		Help:        "Row-level-security violations scraped from rls_violation_log (RLS-6, sampled 1% at the source), by violation_type.",
		ConstLabels: platformLabels,
	}, []string{"violation_type"})

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
		DependencyRequestDuration,
		DuplicateMessagesTotal,
		RLSViolationsPlatformTotal,
	)

	// Pre-initialize labels so dashboards show 0 rather than "no data".
	RLSViolationsTotal.WithLabelValues("missing_or_invalid_guc")
	RLSViolationsTotal.WithLabelValues("cross_tenant_access")
	RLSViolationsPlatformTotal.WithLabelValues("missing_or_invalid_guc")
	RLSViolationsPlatformTotal.WithLabelValues("cross_tenant_access")
}
