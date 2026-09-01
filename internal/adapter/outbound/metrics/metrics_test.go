package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegister_RegistersAllSeventeenInstrumentsOnce exercises Register
// against a real registry once (registered instruments are process-global
// vars, so a second MustRegister of the same collector would panic — see
// TestRegister_SecondCallPanics below, which documents that the "call once
// at process startup" doc comment is a discipline, not enforced idempotency).
func TestRegister_RegistersAllSeventeenInstrumentsOnce(t *testing.T) {
	reg := prometheus.NewRegistry()
	collectors := allCollectors()
	require.Len(t, collectors, 17, "test fixture must track every documented iam_realm_provisioner_* instrument")
	seen := map[string]bool{}
	for _, c := range collectors {
		require.NoError(t, reg.Register(c), "each instrument must register without a name/descriptor collision")
		ch := make(chan *prometheus.Desc, 1)
		c.Describe(ch)
		close(ch)
		for d := range ch {
			assert.False(t, seen[d.String()], "duplicate metric descriptor: %s", d)
			seen[d.String()] = true
		}
	}
}

// TestRegister_AgainstDefaultRegistryPanicsOnRepeat pins down that Register
// itself wires into prometheus.DefaultRegisterer and that calling it twice
// (e.g. from two composition roots in one process) panics rather than
// silently no-op'ing, despite the doc comment's "idempotent-safe" claim.
func TestRegister_AgainstDefaultRegistryPanicsOnRepeat(t *testing.T) {
	Register()
	assert.Panics(t, Register)
}

func allCollectors() []prometheus.Collector {
	return []prometheus.Collector{
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
	}
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
	ProvisionTotal.Reset()
	ProvisionTotal.WithLabelValues("trial", "success").Inc()
	ProvisionTotal.WithLabelValues("trial", "success").Inc()
	ProvisionTotal.WithLabelValues("direct", "failure").Inc()

	assert.Equal(t, float64(2), counterValue(t, ProvisionTotal.WithLabelValues("trial", "success")))
	assert.Equal(t, float64(1), counterValue(t, ProvisionTotal.WithLabelValues("direct", "failure")))
	assert.Equal(t, float64(0), counterValue(t, ProvisionTotal.WithLabelValues("convert", "success")))
}

func TestUsersMigratedTotal_AddsCumulativeCount(t *testing.T) {
	before := counterValue(t, UsersMigratedTotal)
	UsersMigratedTotal.Add(7)
	assert.Equal(t, before+7, counterValue(t, UsersMigratedTotal))
}

func TestRealmActiveGauge_SetByStrategyLabel(t *testing.T) {
	RealmActiveGauge.Reset()
	RealmActiveGauge.WithLabelValues("shared").Set(3)
	RealmActiveGauge.WithLabelValues("dedicated").Set(11)

	assert.Equal(t, float64(3), gaugeValue(t, RealmActiveGauge.WithLabelValues("shared")))
	assert.Equal(t, float64(11), gaugeValue(t, RealmActiveGauge.WithLabelValues("dedicated")))
}

func TestKeycloakCallDuration_ObservesByOpLabel(t *testing.T) {
	KeycloakCallDuration.Reset()
	KeycloakCallDuration.WithLabelValues("create_realm").Observe(0.25)
	KeycloakCallDuration.WithLabelValues("create_realm").Observe(0.75)

	h := histogramSample(t, KeycloakCallDuration.WithLabelValues("create_realm"))
	assert.Equal(t, uint64(2), h.GetSampleCount())
	assert.InDelta(t, 1.0, h.GetSampleSum(), 1e-9)
}

func TestKeycloakCallFailuresTotal_IncrementsByOpLabel(t *testing.T) {
	KeycloakCallFailuresTotal.Reset()
	KeycloakCallFailuresTotal.WithLabelValues("migrate_user").Inc()
	assert.Equal(t, float64(1), counterValue(t, KeycloakCallFailuresTotal.WithLabelValues("migrate_user")))
}

func TestAdminTokenRefreshTotal_IncrementsByTriggerLabel(t *testing.T) {
	AdminTokenRefreshTotal.Reset()
	AdminTokenRefreshTotal.WithLabelValues("proactive").Inc()
	AdminTokenRefreshTotal.WithLabelValues("401").Inc()
	AdminTokenRefreshTotal.WithLabelValues("401").Inc()

	assert.Equal(t, float64(1), counterValue(t, AdminTokenRefreshTotal.WithLabelValues("proactive")))
	assert.Equal(t, float64(2), counterValue(t, AdminTokenRefreshTotal.WithLabelValues("401")))
}

func TestIdpValidationTotal_IncrementsByOutcomeLabel(t *testing.T) {
	IdpValidationTotal.Reset()
	IdpValidationTotal.WithLabelValues("warnings_only").Inc()
	assert.Equal(t, float64(1), counterValue(t, IdpValidationTotal.WithLabelValues("warnings_only")))
	assert.Equal(t, float64(0), counterValue(t, IdpValidationTotal.WithLabelValues("failing")))
}

func TestMfaRoleSyncTotal_IncrementsByActionLabel(t *testing.T) {
	MfaRoleSyncTotal.Reset()
	MfaRoleSyncTotal.WithLabelValues("assign").Inc()
	MfaRoleSyncTotal.WithLabelValues("revoke").Inc()
	MfaRoleSyncTotal.WithLabelValues("revoke").Inc()

	assert.Equal(t, float64(1), counterValue(t, MfaRoleSyncTotal.WithLabelValues("assign")))
	assert.Equal(t, float64(2), counterValue(t, MfaRoleSyncTotal.WithLabelValues("revoke")))
}

func TestRealmConfigPendingGauge_Sets(t *testing.T) {
	RealmConfigPendingGauge.Set(4)
	assert.Equal(t, float64(4), gaugeValue(t, RealmConfigPendingGauge))
	RealmConfigPendingGauge.Set(0)
	assert.Equal(t, float64(0), gaugeValue(t, RealmConfigPendingGauge))
}

func TestTrialExpiryProcessedTotal_Increments(t *testing.T) {
	before := counterValue(t, TrialExpiryProcessedTotal)
	TrialExpiryProcessedTotal.Inc()
	assert.Equal(t, before+1, counterValue(t, TrialExpiryProcessedTotal))
}

// TestTrialExpiryDeferredTotal_IncrementsOnEveryDefer pins down the
// operations-doc invariant (§11.1): this counter must increment on every
// defer, not merely exist, or the "sustained >0" alert can never fire.
func TestTrialExpiryDeferredTotal_IncrementsOnEveryDefer(t *testing.T) {
	before := counterValue(t, TrialExpiryDeferredTotal)
	TrialExpiryDeferredTotal.Inc()
	TrialExpiryDeferredTotal.Inc()
	assert.Equal(t, before+2, counterValue(t, TrialExpiryDeferredTotal))
}

func TestLapseProcessedTotal_IncrementsByTransitionLabel(t *testing.T) {
	LapseProcessedTotal.Reset()
	LapseProcessedTotal.WithLabelValues("suspend").Inc()
	LapseProcessedTotal.WithLabelValues("offboard").Inc()
	LapseProcessedTotal.WithLabelValues("offboard").Inc()

	assert.Equal(t, float64(1), counterValue(t, LapseProcessedTotal.WithLabelValues("suspend")))
	assert.Equal(t, float64(2), counterValue(t, LapseProcessedTotal.WithLabelValues("offboard")))
}

func TestRealmExportTotal_IncrementsByResultLabel(t *testing.T) {
	RealmExportTotal.Reset()
	RealmExportTotal.WithLabelValues("success").Inc()
	RealmExportTotal.WithLabelValues("fail").Inc()

	assert.Equal(t, float64(1), counterValue(t, RealmExportTotal.WithLabelValues("success")))
	assert.Equal(t, float64(1), counterValue(t, RealmExportTotal.WithLabelValues("fail")))
}

func TestUserDeleteTotal_IncrementsByResultLabel(t *testing.T) {
	UserDeleteTotal.Reset()
	UserDeleteTotal.WithLabelValues("success").Inc()
	assert.Equal(t, float64(1), counterValue(t, UserDeleteTotal.WithLabelValues("success")))
}

func TestSessionRevokeTotal_IncrementsByResultLabel(t *testing.T) {
	SessionRevokeTotal.Reset()
	SessionRevokeTotal.WithLabelValues("success").Inc()
	assert.Equal(t, float64(1), counterValue(t, SessionRevokeTotal.WithLabelValues("success")))
}

func TestMfaResetTotal_IncrementsByResultLabel(t *testing.T) {
	MfaResetTotal.Reset()
	MfaResetTotal.WithLabelValues("failure").Inc()
	assert.Equal(t, float64(1), counterValue(t, MfaResetTotal.WithLabelValues("failure")))
}

func TestTemplateDrift_SetsByRealmVersionLabel(t *testing.T) {
	TemplateDrift.Reset()
	TemplateDrift.WithLabelValues("v1").Set(2)
	TemplateDrift.WithLabelValues("v2").Set(0)

	assert.Equal(t, float64(2), gaugeValue(t, TemplateDrift.WithLabelValues("v1")))
	assert.Equal(t, float64(0), gaugeValue(t, TemplateDrift.WithLabelValues("v2")))
}
