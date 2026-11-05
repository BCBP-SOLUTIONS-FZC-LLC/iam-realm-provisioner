package metrics

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/obsregistry"
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
	// Call it before Register() so collectors pick up {domain, service, environment}
	// const labels and land on gincommon's registerer — the same order
	// cmd/server/main.go uses.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		Domain:       "iam",
		Environment:  "test",
		ServiceName:  "iam-realm-provisioner",
		BuildVersion: "test",
	})
	os.Exit(m.Run())
}

func ensureRegistered(t testing.TB) {
	t.Helper()
	Register()
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
	assert.NotNil(t, OrphanedSignupReapTotal)
	assert.NotNil(t, LapseProcessedTotal)
	assert.NotNil(t, RealmExportTotal)
	assert.NotNil(t, UserDeleteTotal)
	assert.NotNil(t, SessionRevokeTotal)
	assert.NotNil(t, MfaResetTotal)
	assert.NotNil(t, AuditEntryDeliveryTotal)
	assert.NotNil(t, AuditEntryBacklogGauge)
	assert.NotNil(t, TemplateDrift)
	assert.NotNil(t, RLSViolationsTotal)
	assert.NotNil(t, UnknownEventAcknowledged)
	assert.NotNil(t, ConsumedSchemaViolationsTotal)
	assert.NotNil(t, ProcessedEventsDuplicates)
	assert.NotNil(t, DependencyRequestDuration)
}

func TestRegister_IsIdempotent(t *testing.T) {
	ensureRegistered(t)
	assert.NotPanics(t, func() { Register() })
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
	assert.Equal(t, "iam", got["domain"])
	assert.Equal(t, "iam-realm-provisioner", got["service"])
	assert.Equal(t, "test", got["environment"])
	assert.NotContains(t, got, "version", "version is only a label on platform_build_info")

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
		assert.Equal(t, "iam", labels["domain"])
		assert.Equal(t, "iam-realm-provisioner", labels["service"])
		assert.Equal(t, "test", labels["environment"])
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
	DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "error").Observe(1.5)

	ok := histogramSample(t, DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "success"))
	assert.Equal(t, uint64(2), ok.GetSampleCount())
	assert.InDelta(t, 1.0, ok.GetSampleSum(), 1e-9)

	failed := histogramSample(t, DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "error"))
	assert.Equal(t, uint64(1), failed.GetSampleCount())
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
	assert.Equal(t, "iam-realm-provisioner", labels["service"])
	assert.Equal(t, "test", labels["environment"])
}

// --- Naming-convention CI enforcement (Platform Observability Standard) --

// TestNamingConvention_ConformsToPlatformObservabilityStandard is this
// repo's CI guardrail for the Enterprise Platform Observability Standard.
// It gathers every collector actually registered on gincommon's registerer
// (gincommon's own platform_http_*/platform_build_info, this package's
// Tier 3 iam_realm_provisioner_* metrics and the shared
// platform_dependency_request_seconds) and validates the result against the
// central Platform Observability Registry (pkg/obsregistry): registry
// names, label sets and vocabularies, required {domain, service,
// environment} labels, the iam_realm_provisioner_ prefix, suffixes and
// prohibited high-cardinality labels. Removed (pre-standard) names are
// errors. Proposed metrics are warnings, not errors (Options.Strict=false):
// the registry's successors are all still awaiting ratification.
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
	AuditEntryDeliveryTotal.WithLabelValues("delivered").Add(0)
	AuditEntryBacklogGauge.WithLabelValues("pending").Add(0)
	ServiceAccountRegisterTotal.WithLabelValues("success").Add(0)
	ServiceAccountKeyRefreshTotal.WithLabelValues("success").Add(0)
	OpenBaoErrorsTotal.WithLabelValues("write_secret", "error").Add(0)
	TrialHardDeleteTotal.WithLabelValues("succeeded").Add(0)
	PruneRowsTotal.WithLabelValues("outbox", "outbox_events").Add(0)
	IdempotencyReplayTotal.WithLabelValues("trial").Add(0)
	CompensationTotal.WithLabelValues("delete_realm", "success").Add(0)
	TemplateDrift.WithLabelValues("v1").Add(0)
	RLSViolationsTotal.WithLabelValues("cross_tenant_access").Add(0)
	UnknownEventAcknowledged.WithLabelValues("topic", "Type").Add(0)
	ConsumedSchemaViolationsTotal.WithLabelValues("billing", "TenantReactivated").Add(0)
	ProcessedEventsDuplicates.WithLabelValues("tenant").Add(0)
	DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "success").Observe(0)
	DependencyRequestDuration.WithLabelValues("keycloak", "token", "error").Observe(0)

	rep, err := obsregistry.Validate(gincommon.MetricsGatherer(), obsregistry.Options{})
	require.NoError(t, err)
	for _, f := range rep.Errors() {
		t.Errorf("Platform Observability Standard violation: %v", f)
	}
	assert.Equal(t, obsregistry.TierService, rep.Tiers[prefix+"provision_total"], "Tier 3 metrics must use the %s prefix", prefix)
	assert.Equal(t, obsregistry.TierPlatform, rep.Tiers["platform_dependency_request_seconds"])

	// Every Tier 3 metric name must be under this service's registry
	// prefix (<domain>_<service>_), which the frozen §25 prefix equals.
	assert.Equal(t, prefix, obsregistry.ServicePrefix("iam", "iam-realm-provisioner"))
	families, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	checked := 0
	for _, f := range families {
		name := f.GetName()
		if !strings.HasPrefix(name, "iam_") {
			continue
		}
		checked++
		assert.True(t, strings.HasPrefix(name, prefix), "%s: service-specific metrics must be named %s<metric>", name, prefix)
	}
	assert.Greater(t, checked, 0, "expected at least one of this service's own metrics to be gathered")
}

// A second registration of a registry histogram reuses the collector that
// is already registered (platform-events registers the same name first).
func TestSharedHistogram_AlreadyRegistered_ReturnsExistingCollector(t *testing.T) {
	Register()
	assert.Same(t, DependencyRequestDuration, sharedHistogram("platform_dependency_request_seconds"))
}

// A name missing from the Platform Observability Registry is a programming
// error and fails at startup.
func TestSharedHistogram_UnregisteredName_Panics(t *testing.T) {
	assert.Panics(t, func() { sharedHistogram("platform_not_in_the_registry_seconds") })
}
