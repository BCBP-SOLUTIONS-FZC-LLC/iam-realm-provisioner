package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
)

// TestStandard_LibrariesShareRegistryCollectors runs the production
// registration order — ObservabilityMiddlewares (TestMain) → Register →
// events.InitMetrics → pgmetrics.InitWithIdentity — and checks that the
// libraries share this service's registry without any metric being
// disabled: platform-events must adopt Register's
// platform_dependency_request_seconds collector, not be refused. With
// METRICS_SCRAPE_OUT set it writes the full /metrics scrape for
// .github/scripts/metricslint.sh (`metricslint check` and the
// docs/observability/metric-registry.md inventory).
func TestStandard_LibrariesShareRegistryCollectors(t *testing.T) {
	ensureRegistered(t)

	identity := gincommon.MetricsConstLabels()
	evWarnings, err := events.InitMetrics(events.MetricsIdentityFromLabels(identity), gincommon.MetricsRegisterer())
	require.NoError(t, err)
	assert.Empty(t, evWarnings, "no platform-events metric may be disabled (platform_dependency_request_seconds must be adopted)")

	pgWarnings, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(identity), gincommon.MetricsRegisterer())
	require.NoError(t, err)
	assert.Empty(t, pgWarnings, "no platform-pgcommon metric may be disabled")

	exerciseEveryCollector()

	gathered, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range gathered {
		names[mf.GetName()] = true
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "version" && (strings.HasPrefix(mf.GetName(), "platform_") || strings.HasPrefix(mf.GetName(), "iam_")) {
					assert.Equal(t, "platform_build_info", mf.GetName(), "version label only on platform_build_info")
				}
			}
		}
	}
	for _, want := range []string{"platform_build_info", "platform_library_info", "platform_dependency_request_seconds", "iam_realm_provisioner_provision_total"} {
		assert.True(t, names[want], "%s must be exported", want)
	}

	if out := os.Getenv("METRICS_SCRAPE_OUT"); out != "" {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
		req.Header.Set("Accept", "text/plain")
		gincommon.MetricsHandler().ServeHTTP(rec, req) // the production /metrics handler
		require.NoError(t, os.WriteFile(out, rec.Body.Bytes(), 0o600))
	}
}

// exerciseEveryCollector records one sample with representative production
// label values on every collector this package owns, so the scrape written
// for `metricslint check` exposes every label set (an unsampled vec has no
// series, and its label values would go unchecked).
func exerciseEveryCollector() {
	ProvisionTotal.WithLabelValues("trial", "success").Add(0)
	UsersMigratedTotal.Add(0)
	RealmActiveGauge.WithLabelValues("shared").Set(0)
	KeycloakCallDuration.WithLabelValues("create_realm").Observe(0.01)
	KeycloakCallFailuresTotal.WithLabelValues("create_realm").Add(0)
	AdminTokenRefreshTotal.WithLabelValues("proactive").Add(0)
	IdpValidationTotal.WithLabelValues("pass").Add(0)
	MfaRoleSyncTotal.WithLabelValues("assign").Add(0)
	RealmConfigPendingGauge.Set(0)
	TrialExpiryProcessedTotal.Add(0)
	TrialExpiryDeferredTotal.Add(0)
	LapseProcessedTotal.WithLabelValues("suspend").Add(0)
	RealmExportTotal.WithLabelValues("success").Add(0)
	UserDeleteTotal.WithLabelValues("success").Add(0)
	SessionRevokeTotal.WithLabelValues("success").Add(0)
	MfaResetTotal.WithLabelValues("success").Add(0)
	AuditEntryDeliveryTotal.WithLabelValues("delivered").Add(0)
	AuditEntryBacklogGauge.WithLabelValues("pending").Set(0)
	ServiceAccountRegisterTotal.WithLabelValues("success").Add(0)
	ServiceAccountKeyRefreshTotal.WithLabelValues("success").Add(0)
	ServiceAccountBackfillTotal.WithLabelValues("success").Add(0)
	TemplateDrift.WithLabelValues("v1").Set(0)
	RLSViolationsTotal.WithLabelValues("cross_tenant_access").Add(0)
	UnknownEventAcknowledged.WithLabelValues("iam.tenant.events", "SomethingNew").Add(0)
	ConsumedSchemaViolationsTotal.WithLabelValues("billing", "TenantReactivated").Add(0)
	ProcessedEventsDuplicates.WithLabelValues("tenant").Add(0)
	DependencyRequestDuration.WithLabelValues("keycloak", "create_realm", "success").Observe(0.01)
}
