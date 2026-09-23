package metrics

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestMain(m *testing.M) {
	// gincommon's metrics.Init is a sync.Once singleton — once
	// ObservabilityMiddlewares runs below, gincommonLabels() returns
	// non-empty for the rest of this process, forever. This is the one and
	// only point where gincommonLabels()'s "not bootstrapped yet" nil-return
	// branch (its own doc comment's stated contract) can be observed, so
	// check it here rather than in a Test* function that could run after
	// bootstrap in any order.
	if got := gincommonLabels(); got != nil {
		fmt.Fprintf(os.Stderr, "gincommonLabels() must return nil before gincommon.ObservabilityMiddlewares has run, got %v\n", got)
		os.Exit(1)
	}

	// ObservabilityMiddlewares is gincommon's public metrics-init API.
	// Call it before Register() so collectors pick up {service, version}
	// const labels and land on gincommon's registerer — the same order
	// cmd/server/main.go uses.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-realm-provisioner",
		BuildVersion: "test",
	})
	os.Exit(m.Run())
}

func ensureRegistered(t testing.TB) {
	t.Helper()
	Register("test")
}

func TestRegister_SucceedsAndPopulatesAllVars(t *testing.T) {
	ensureRegistered(t)

	assert.NotNil(t, ProvisionTotal)
	assert.NotNil(t, UsersMigratedTotal)
	assert.NotNil(t, RealmActiveGauge)
	assert.NotNil(t, KeycloakCallDuration)
	assert.NotNil(t, KeycloakCallFailuresTotal)
	assert.NotNil(t, AdminTokenRefreshTotal)
	assert.NotNil(t, IdpValidationTotal)
	assert.NotNil(t, MfaRoleSyncTotal)
	assert.NotNil(t, RealmConfigPendingGauge)
	assert.NotNil(t, TrialExpiryProcessedTotal)
	assert.NotNil(t, TrialExpiryDeferredTotal)
	assert.NotNil(t, LapseProcessedTotal)
	assert.NotNil(t, RealmExportTotal)
	assert.NotNil(t, UserDeleteTotal)
	assert.NotNil(t, SessionRevokeTotal)
	assert.NotNil(t, MfaResetTotal)
	assert.NotNil(t, TemplateDrift)
	assert.NotNil(t, RLSViolationsTotal)
	assert.NotNil(t, UnknownEventAcknowledged)
	assert.NotNil(t, ConsumedSchemaViolationsTotal)
	assert.NotNil(t, ProcessedEventsDuplicates)
	assert.NotNil(t, DependencyRequestDuration)
	assert.NotNil(t, DuplicateMessagesTotal)
	assert.NotNil(t, RLSViolationsPlatformTotal)
}

func TestRegister_IsIdempotent(t *testing.T) {
	ensureRegistered(t)
	assert.NotPanics(t, func() { Register("test") })
}

func TestRegister_LandsOnGincommonRegisterer(t *testing.T) {
	ensureRegistered(t)

	ProvisionTotal.WithLabelValues("trial", "success").Inc()
	KeycloakCallDuration.WithLabelValues("create_realm").Observe(0.01)

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	assert.True(t, names[prefix+"provision_total"], "collectors must land on gincommon's registerer (DefaultRegisterer after ObservabilityMiddlewares)")
	assert.True(t, names[prefix+"keycloak_call_duration_seconds"])
}

func TestRegister_AppliesGincommonConstLabels(t *testing.T) {
	ensureRegistered(t)

	got := gincommonLabels()
	want := gincommon.MetricsConstLabels()
	assert.Equal(t, want, got)
	assert.Equal(t, "iam-realm-provisioner", got["service"])
	assert.Equal(t, "test", got["version"])

	ProvisionTotal.WithLabelValues("trial", "success").Inc()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if f.GetName() != prefix+"provision_total" {
			continue
		}
		found = true
		require.NotEmpty(t, f.GetMetric())
		labels := map[string]string{}
		for _, lp := range f.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		assert.Equal(t, "iam-realm-provisioner", labels["service"])
		assert.Equal(t, "test", labels["version"])
	}
	assert.True(t, found, prefix+"provision_total must be gathered")
}

func TestPreseededRLSLabelsPresent(t *testing.T) {
	ensureRegistered(t)

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var rlsMf *dto.MetricFamily
	for _, mf := range gathered {
		if mf.GetName() == prefix+"rls_violations_total" {
			rlsMf = mf
			break
		}
	}
	require.NotNil(t, rlsMf, prefix+"rls_violations_total must be gathered")

	seenTypes := map[string]bool{}
	for _, m := range rlsMf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "violation_type" {
				seenTypes[lp.GetValue()] = true
			}
		}
	}
	assert.True(t, seenTypes["missing_or_invalid_guc"])
	assert.True(t, seenTypes["cross_tenant_access"])
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, g.Write(&m))
	return m.GetGauge().GetValue()
}

func histogramSample(t *testing.T, o prometheus.Observer) *dto.Histogram {
	t.Helper()
	h, ok := o.(prometheus.Histogram)
	require.True(t, ok, "HistogramVec.WithLabelValues must yield a prometheus.Histogram")
	var m dto.Metric
	require.NoError(t, h.Write(&m))
	return m.GetHistogram()
}

func TestProvisionTotal_IncrementsByKindAndResultLabels(t *testing.T) {
	ensureRegistered(t)
	ProvisionTotal.Reset()
	ProvisionTotal.WithLabelValues("trial", "success").Inc()
	ProvisionTotal.WithLabelValues("trial", "success").Inc()
	ProvisionTotal.WithLabelValues("direct", "failure").Inc()

	assert.Equal(t, float64(2), counterValue(t, ProvisionTotal.WithLabelValues("trial", "success")))
	assert.Equal(t, float64(1), counterValue(t, ProvisionTotal.WithLabelValues("direct", "failure")))
	assert.Equal(t, float64(0), counterValue(t, ProvisionTotal.WithLabelValues("convert", "success")))
}

func TestUsersMigratedTotal_AddsCumulativeCount(t *testing.T) {
	ensureRegistered(t)
	before := counterValue(t, UsersMigratedTotal)
	UsersMigratedTotal.Add(7)
	assert.Equal(t, before+7, counterValue(t, UsersMigratedTotal))
}

func TestRealmActiveGauge_SetByStrategyLabel(t *testing.T) {
	ensureRegistered(t)
	RealmActiveGauge.Reset()
	RealmActiveGauge.WithLabelValues("shared").Set(3)
	RealmActiveGauge.WithLabelValues("dedicated").Set(11)

	assert.Equal(t, float64(3), gaugeValue(t, RealmActiveGauge.WithLabelValues("shared")))
	assert.Equal(t, float64(11), gaugeValue(t, RealmActiveGauge.WithLabelValues("dedicated")))
}

func TestKeycloakCallDuration_ObservesByOpLabel(t *testing.T) {
	ensureRegistered(t)
	KeycloakCallDuration.Reset()
	KeycloakCallDuration.WithLabelValues("create_realm").Observe(0.25)
	KeycloakCallDuration.WithLabelValues("create_realm").Observe(0.75)

	h := histogramSample(t, KeycloakCallDuration.WithLabelValues("create_realm"))
	assert.Equal(t, uint64(2), h.GetSampleCount())
	assert.InDelta(t, 1.0, h.GetSampleSum(), 1e-9)
}

func TestKeycloakCallFailuresTotal_IncrementsByOpLabel(t *testing.T) {
	ensureRegistered(t)
	KeycloakCallFailuresTotal.Reset()
	KeycloakCallFailuresTotal.WithLabelValues("migrate_user").Inc()
	assert.Equal(t, float64(1), counterValue(t, KeycloakCallFailuresTotal.WithLabelValues("migrate_user")))
}

func TestAdminTokenRefreshTotal_IncrementsByTriggerLabel(t *testing.T) {
	ensureRegistered(t)
	AdminTokenRefreshTotal.Reset()
	AdminTokenRefreshTotal.WithLabelValues("proactive").Inc()
	AdminTokenRefreshTotal.WithLabelValues("401").Inc()
	AdminTokenRefreshTotal.WithLabelValues("401").Inc()

	assert.Equal(t, float64(1), counterValue(t, AdminTokenRefreshTotal.WithLabelValues("proactive")))
	assert.Equal(t, float64(2), counterValue(t, AdminTokenRefreshTotal.WithLabelValues("401")))
}

func TestIdpValidationTotal_IncrementsByOutcomeLabel(t *testing.T) {
	ensureRegistered(t)
	IdpValidationTotal.Reset()
	IdpValidationTotal.WithLabelValues("warnings_only").Inc()
	assert.Equal(t, float64(1), counterValue(t, IdpValidationTotal.WithLabelValues("warnings_only")))
	assert.Equal(t, float64(0), counterValue(t, IdpValidationTotal.WithLabelValues("failing")))
}

func TestMfaRoleSyncTotal_IncrementsByActionLabel(t *testing.T) {
	ensureRegistered(t)
	MfaRoleSyncTotal.Reset()
	MfaRoleSyncTotal.WithLabelValues("assign").Inc()
	MfaRoleSyncTotal.WithLabelValues("revoke").Inc()
	MfaRoleSyncTotal.WithLabelValues("revoke").Inc()

	assert.Equal(t, float64(1), counterValue(t, MfaRoleSyncTotal.WithLabelValues("assign")))
	assert.Equal(t, float64(2), counterValue(t, MfaRoleSyncTotal.WithLabelValues("revoke")))
}

func TestRealmConfigPendingGauge_Sets(t *testing.T) {
	ensureRegistered(t)
	RealmConfigPendingGauge.Set(4)
	assert.Equal(t, float64(4), gaugeValue(t, RealmConfigPendingGauge))
	RealmConfigPendingGauge.Set(0)
	assert.Equal(t, float64(0), gaugeValue(t, RealmConfigPendingGauge))
}

func TestTrialExpiryProcessedTotal_Increments(t *testing.T) {
	ensureRegistered(t)
	before := counterValue(t, TrialExpiryProcessedTotal)
	TrialExpiryProcessedTotal.Inc()
	assert.Equal(t, before+1, counterValue(t, TrialExpiryProcessedTotal))
}

func TestTrialExpiryDeferredTotal_IncrementsOnEveryDefer(t *testing.T) {
	ensureRegistered(t)
	before := counterValue(t, TrialExpiryDeferredTotal)
	TrialExpiryDeferredTotal.Inc()
	TrialExpiryDeferredTotal.Inc()
	assert.Equal(t, before+2, counterValue(t, TrialExpiryDeferredTotal))
}

func TestLapseProcessedTotal_IncrementsByTransitionLabel(t *testing.T) {
	ensureRegistered(t)
	LapseProcessedTotal.Reset()
	LapseProcessedTotal.WithLabelValues("suspend").Inc()
	LapseProcessedTotal.WithLabelValues("offboard").Inc()
	LapseProcessedTotal.WithLabelValues("offboard").Inc()

	assert.Equal(t, float64(1), counterValue(t, LapseProcessedTotal.WithLabelValues("suspend")))
	assert.Equal(t, float64(2), counterValue(t, LapseProcessedTotal.WithLabelValues("offboard")))
}

func TestRLSViolationsTotal_IncrementsByViolationTypeLabel(t *testing.T) {
	ensureRegistered(t)
	RLSViolationsTotal.Reset()
	RLSViolationsTotal.WithLabelValues("cross_tenant_access").Add(2)
	RLSViolationsTotal.WithLabelValues("missing_or_invalid_guc").Inc()

	assert.Equal(t, float64(2), counterValue(t, RLSViolationsTotal.WithLabelValues("cross_tenant_access")))
	assert.Equal(t, float64(1), counterValue(t, RLSViolationsTotal.WithLabelValues("missing_or_invalid_guc")))
}

func TestRealmExportTotal_IncrementsByResultLabel(t *testing.T) {
	ensureRegistered(t)
	RealmExportTotal.Reset()
	RealmExportTotal.WithLabelValues("success").Inc()
	RealmExportTotal.WithLabelValues("fail").Inc()

	assert.Equal(t, float64(1), counterValue(t, RealmExportTotal.WithLabelValues("success")))
	assert.Equal(t, float64(1), counterValue(t, RealmExportTotal.WithLabelValues("fail")))
}

func TestUserDeleteTotal_IncrementsByResultLabel(t *testing.T) {
	ensureRegistered(t)
	UserDeleteTotal.Reset()
	UserDeleteTotal.WithLabelValues("success").Inc()
	assert.Equal(t, float64(1), counterValue(t, UserDeleteTotal.WithLabelValues("success")))
}

func TestSessionRevokeTotal_IncrementsByResultLabel(t *testing.T) {
	ensureRegistered(t)
	SessionRevokeTotal.Reset()
	SessionRevokeTotal.WithLabelValues("success").Inc()
	assert.Equal(t, float64(1), counterValue(t, SessionRevokeTotal.WithLabelValues("success")))
}

func TestMfaResetTotal_IncrementsByResultLabel(t *testing.T) {
	ensureRegistered(t)
	MfaResetTotal.Reset()
	MfaResetTotal.WithLabelValues("failure").Inc()
	assert.Equal(t, float64(1), counterValue(t, MfaResetTotal.WithLabelValues("failure")))
}

func TestTemplateDrift_SetsByRealmVersionLabel(t *testing.T) {
	ensureRegistered(t)
	TemplateDrift.Reset()
	TemplateDrift.WithLabelValues("v1").Set(2)
	TemplateDrift.WithLabelValues("v2").Set(0)

	assert.Equal(t, float64(2), gaugeValue(t, TemplateDrift.WithLabelValues("v1")))
	assert.Equal(t, float64(0), gaugeValue(t, TemplateDrift.WithLabelValues("v2")))
}

func TestUnknownEventAcknowledged_IncrementsByTopicAndType(t *testing.T) {
	ensureRegistered(t)
	UnknownEventAcknowledged.Reset()
	UnknownEventAcknowledged.WithLabelValues("membership", "DepartmentMembershipRevoked").Inc()
	assert.Equal(t, float64(1), counterValue(t, UnknownEventAcknowledged.WithLabelValues("membership", "DepartmentMembershipRevoked")))
}

func TestProcessedEventsDuplicates_IncrementsByConsumer(t *testing.T) {
	ensureRegistered(t)
	ProcessedEventsDuplicates.Reset()
	ProcessedEventsDuplicates.WithLabelValues("tenant").Inc()
	ProcessedEventsDuplicates.WithLabelValues("tenant").Inc()
	assert.Equal(t, float64(2), counterValue(t, ProcessedEventsDuplicates.WithLabelValues("tenant")))
	assert.Equal(t, float64(0), counterValue(t, ProcessedEventsDuplicates.WithLabelValues("billing")))
}

// --- Tier 1 (platform_*) metrics ---------------------------------------

func TestDependencyRequestDuration_ObservesByDependencyOperationOutcome(t *testing.T) {
	ensureRegistered(t)
	DependencyRequestDuration.Reset()
	DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "success").Observe(0.25)
	DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "success").Observe(0.75)
	DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "failure").Observe(1.5)

	ok := histogramSample(t, DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "success"))
	assert.Equal(t, uint64(2), ok.GetSampleCount())
	assert.InDelta(t, 1.0, ok.GetSampleSum(), 1e-9)

	failed := histogramSample(t, DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "failure"))
	assert.Equal(t, uint64(1), failed.GetSampleCount())
}

func TestDuplicateMessagesTotal_IncrementsByConsumer(t *testing.T) {
	ensureRegistered(t)
	DuplicateMessagesTotal.Reset()
	DuplicateMessagesTotal.WithLabelValues("tenant").Inc()
	DuplicateMessagesTotal.WithLabelValues("tenant").Inc()
	assert.Equal(t, float64(2), counterValue(t, DuplicateMessagesTotal.WithLabelValues("tenant")))
	assert.Equal(t, float64(0), counterValue(t, DuplicateMessagesTotal.WithLabelValues("billing")))
}

func TestRLSViolationsPlatformTotal_IncrementsByViolationTypeLabel(t *testing.T) {
	ensureRegistered(t)
	RLSViolationsPlatformTotal.Reset()
	RLSViolationsPlatformTotal.WithLabelValues("cross_tenant_access").Add(2)
	RLSViolationsPlatformTotal.WithLabelValues("missing_or_invalid_guc").Inc()

	assert.Equal(t, float64(2), counterValue(t, RLSViolationsPlatformTotal.WithLabelValues("cross_tenant_access")))
	assert.Equal(t, float64(1), counterValue(t, RLSViolationsPlatformTotal.WithLabelValues("missing_or_invalid_guc")))
}

func TestPreseededRLSPlatformLabelsPresent(t *testing.T) {
	ensureRegistered(t)

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var mf *dto.MetricFamily
	for _, f := range gathered {
		if f.GetName() == "platform_rls_violations_total" {
			mf = f
			break
		}
	}
	require.NotNil(t, mf, "platform_rls_violations_total must be gathered")

	seenTypes := map[string]bool{}
	for _, m := range mf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "violation_type" {
				seenTypes[lp.GetValue()] = true
			}
		}
	}
	assert.True(t, seenTypes["missing_or_invalid_guc"])
	assert.True(t, seenTypes["cross_tenant_access"])
}

func TestPlatformConstLabels_CarryDomainServiceEnvironment(t *testing.T) {
	ensureRegistered(t)

	DependencyRequestDuration.WithLabelValues("keycloak", "test_op", "success").Observe(0.01)

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var mf *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "platform_dependency_request_seconds" {
			mf = f
			break
		}
	}
	require.NotNil(t, mf, "platform_dependency_request_seconds must be gathered")
	require.NotEmpty(t, mf.GetMetric())

	labels := map[string]string{}
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	assert.Equal(t, "iam", labels["domain"])
	assert.Equal(t, "realm-provisioner", labels["service"])
	assert.Equal(t, "test", labels["environment"])
}

// --- Naming-convention CI enforcement (Platform Observability Standard) --

// tierOf classifies a metric name per the standard's decision tree. Tier 2
// (iam_*, domain-shared) is name-ambiguous with Tier 3 (both start with
// "iam_"), so it is disambiguated by the frozen service-specific prefix.
func tierOf(name string) string {
	switch {
	case strings.HasPrefix(name, "platform_"):
		return "platform"
	case strings.HasPrefix(name, prefix): // iam_realm_provisioner_
		return "service"
	case strings.HasPrefix(name, "iam_"):
		return "domain"
	default:
		return "unknown"
	}
}

var (
	counterSuffix   = regexp.MustCompile(`_total$`)
	histogramSuffix = regexp.MustCompile(`_seconds$`)
)

// registryByName indexes PlatformRegistry for TestNamingConvention_*'s
// registry-compliance and label-vocabulary checks (rules 9-12 and Label
// Governance: shared metric names/labels must come from the approved
// registry, not be invented ad hoc by instrumentation code).
func registryByName() map[string]RegistryEntry {
	m := make(map[string]RegistryEntry, len(PlatformRegistry))
	for _, e := range PlatformRegistry {
		m[e.Name] = e
	}
	return m
}

// TestNamingConvention_ConformsToPlatformObservabilityStandard is this
// repo's CI guardrail for the Enterprise Platform Observability Standard:
// every counter this service registers must end in _total, every
// histogram in _seconds, a domain-shared (iam_*, non-service-specific)
// metric must never encode this service's name, and — per rules 9-12 and
// Label Governance — every platform_* (Tier 1) metric must (a) carry
// {domain, service, environment}, (b) have a corresponding PlatformRegistry
// entry (a platform_* name with no registry entry is exactly the
// unilateral invention rule 11 forbids), and (c) use only labels that
// entry's RequiredLabels/ApprovedLabels lists (label vocabulary
// compliance). It runs against what's actually gathered off gincommon's
// registerer, not a hand-copied list, so a future metric added to
// metrics.go without following the standard fails this test.
func TestNamingConvention_ConformsToPlatformObservabilityStandard(t *testing.T) {
	ensureRegistered(t)

	// Force at least one sample on every dynamically-labeled collector so
	// it is present in Gather() output regardless of test execution order
	// (a *Vec with no WithLabelValues call yet produces zero samples).
	ProvisionTotal.WithLabelValues("trial", "success").Add(0)
	RealmActiveGauge.WithLabelValues("shared").Add(0)
	KeycloakCallDuration.WithLabelValues("create_realm").Observe(0)
	KeycloakCallFailuresTotal.WithLabelValues("create_realm").Add(0)
	AdminTokenRefreshTotal.WithLabelValues("proactive").Add(0)
	IdpValidationTotal.WithLabelValues("pass").Add(0)
	MfaRoleSyncTotal.WithLabelValues("assign").Add(0)
	LapseProcessedTotal.WithLabelValues("suspend").Add(0)
	RealmExportTotal.WithLabelValues("success").Add(0)
	UserDeleteTotal.WithLabelValues("success").Add(0)
	SessionRevokeTotal.WithLabelValues("success").Add(0)
	MfaResetTotal.WithLabelValues("success").Add(0)
	TemplateDrift.WithLabelValues("v1").Add(0)
	RLSViolationsTotal.WithLabelValues("cross_tenant_access").Add(0)
	UnknownEventAcknowledged.WithLabelValues("topic", "Type").Add(0)
	ConsumedSchemaViolationsTotal.WithLabelValues("billing", "TenantReactivated").Add(0)
	ProcessedEventsDuplicates.WithLabelValues("tenant").Add(0)
	DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "success").Observe(0)
	DuplicateMessagesTotal.WithLabelValues("tenant").Add(0)
	RLSViolationsPlatformTotal.WithLabelValues("cross_tenant_access").Add(0)

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	registry := registryByName()
	checked := 0

	for _, f := range families {
		name := f.GetName()
		tier := tierOf(name)
		if tier == "unknown" {
			continue // owned by gincommon/platform-events/pgcommon, not this rename
		}
		checked++

		switch f.GetType() {
		case dto.MetricType_COUNTER:
			assert.True(t, counterSuffix.MatchString(name), "%s: counters must end in _total (Rule 4)", name)
		case dto.MetricType_HISTOGRAM:
			assert.True(t, histogramSuffix.MatchString(name), "%s: histograms must end in _seconds (Rule 5)", name)
		default:
			// Gauges have no mandated suffix rule (Rule 6 is advisory, not
			// a lintable suffix); this service registers no summary/
			// untyped/gauge-histogram metrics.
		}

		if tier == "domain" {
			assert.False(t, strings.Contains(name, "realm_provisioner") || strings.Contains(name, "realm-provisioner"),
				"%s: a domain-shared (iam_*) metric must not encode the service name (Rule 2)", name)
		}

		if tier != "platform" {
			continue
		}
		assert.False(t, strings.Contains(name, "realm_provisioner") || strings.Contains(name, "realm-provisioner"),
			"%s: a platform_* metric must not encode the service name in its name (Rule 1)", name)

		// Registry compliance (rules 9-12): a platform_* name this service
		// emits with no PlatformRegistry entry is exactly the unilateral
		// invention the standard forbids.
		entry, ok := registry[name]
		if !assert.True(t, ok, "%s: platform_* metric has no PlatformRegistry entry (registry.go) — rules 9-12 forbid emitting an unregistered platform_* name", name) {
			continue
		}

		require.NotEmpty(t, f.GetMetric(), "%s: must have at least one sample to check labels", name)
		labelNames := map[string]bool{}
		for _, lp := range f.GetMetric()[0].GetLabel() {
			labelNames[lp.GetName()] = true
		}
		for _, want := range entry.RequiredLabels {
			assert.True(t, labelNames[want], "%s: platform_* metrics require label %q (Tier 1 required labels)", name, want)
		}

		// Label vocabulary compliance: every label actually present must
		// be either a required const label or one this entry's packet
		// requested approval for — a label outside both is an
		// unapproved, ad hoc addition to a shared metric's contract.
		approved := make(map[string]bool, len(entry.RequiredLabels)+len(entry.ApprovedLabels))
		for _, l := range entry.RequiredLabels {
			approved[l] = true
		}
		for _, l := range entry.ApprovedLabels {
			approved[l] = true
		}
		for label := range labelNames {
			assert.True(t, approved[label],
				"%s: label %q is not in this metric's approved vocabulary (RequiredLabels/ApprovedLabels in registry.go) — shared-metric labels must come from the registry, not be invented ad hoc", name, label)
		}
	}

	assert.Greater(t, checked, 0, "expected at least one of this service's own metrics to be gathered")
}

// TestPlatformRegistry_EntriesAreWellFormed guards registry.go itself: an
// entry with no required labels, no semantic definition, or an empty name
// would make TestNamingConvention_ConformsToPlatformObservabilityStandard's
// registry-compliance check vacuous.
func TestPlatformRegistry_EntriesAreWellFormed(t *testing.T) {
	require.NotEmpty(t, PlatformRegistry)
	seen := map[string]bool{}
	for _, e := range PlatformRegistry {
		assert.NotEmpty(t, e.Name)
		assert.False(t, seen[e.Name], "%s: duplicate PlatformRegistry entry", e.Name)
		seen[e.Name] = true
		assert.True(t, strings.HasPrefix(e.Name, "platform_") || e.Tier == "domain", "%s: Tier %q entries must be named platform_* unless Tier is domain", e.Name, e.Tier)
		assert.NotEmpty(t, e.SemanticDefinition, "%s: registry entries must carry a semantic definition (Registry Ratification Requirement)", e.Name)
		assert.NotEmpty(t, e.RequiredLabels, "%s: registry entries must declare required labels", e.Name)
		assert.Contains(t, []RegistryStatus{StatusCanonical, StatusProposed}, e.Status, "%s: unknown status", e.Name)
	}
}
