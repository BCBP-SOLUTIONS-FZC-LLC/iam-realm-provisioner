package main

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func TestAuditEnvLookup_DefaultsSourceService(t *testing.T) {
	t.Setenv(auditconfig.EnvSourceService, "")
	v, ok := auditEnvLookup(auditconfig.EnvSourceService)
	assert.True(t, ok)
	assert.Equal(t, domain.Source, v)

	t.Setenv(auditconfig.EnvSourceService, "other-service")
	v, _ = auditEnvLookup(auditconfig.EnvSourceService)
	assert.Equal(t, "other-service", v, "an explicit value is passed through, then rejected by newAuditOutbox")

	t.Setenv(auditconfig.EnvBaseURL, "http://audit-log:8080")
	v, ok = auditEnvLookup(auditconfig.EnvBaseURL)
	assert.True(t, ok)
	assert.Equal(t, "http://audit-log:8080", v)
}

// Config errors fail before any pool is touched, so nil pools are safe.
func TestNewAuditOutbox_ConfigErrorsAreFatal(t *testing.T) {
	t.Run("malformed AUDIT_* value", func(t *testing.T) {
		t.Setenv(auditconfig.EnvTimeout, "forever")
		_, err := newAuditOutbox(context.Background(), nil, nil, &fakeLogger{})
		require.ErrorIs(t, err, audit.ErrConfig)
	})
	t.Run("app pool access check", func(t *testing.T) {
		ensureMetricsRegistered(t)
		_, err := newAuditOutbox(context.Background(), nil, nil, &fakeLogger{})
		require.ErrorIs(t, err, audit.ErrConfig, "CheckEnqueueAccess refuses a missing pool")
	})
	t.Run("source service other than this service", func(t *testing.T) {
		t.Setenv(auditconfig.EnvSourceService, "iam-org-membership")
		_, err := newAuditOutbox(context.Background(), nil, nil, &fakeLogger{})
		require.ErrorIs(t, err, audit.ErrConfig)
		assert.ErrorContains(t, err, "producer-restricted")
	})
}

func TestLegacyAuditMetrics_MapsOutcomes(t *testing.T) {
	ensureMetricsRegistered(t)
	cases := map[audit.Outcome]string{
		audit.OutcomeDelivered:   "delivered",
		audit.OutcomeRetry:       "retry",
		audit.OutcomeSkew:        "unknown_entry_type",
		audit.OutcomeNotAdmitted: "not_admitted",
		audit.OutcomeRejected:    "rejected",
		audit.OutcomeInvalid:     "rejected",
	}
	for outcome, label := range cases {
		before := testutil.ToFloat64(metrics.AuditEntryDeliveryTotal.WithLabelValues(label))
		legacyAuditMetrics{}.Delivered("config.idp.changed", outcome)
		assert.InDelta(t, before+1, testutil.ToFloat64(metrics.AuditEntryDeliveryTotal.WithLabelValues(label)), 0, "outcome %s", outcome)
	}

	legacyAuditMetrics{}.Backlog(7, 2, time.Minute)
	assert.InDelta(t, 7, testutil.ToFloat64(metrics.AuditEntryBacklogGauge.WithLabelValues("pending")), 0)
	assert.InDelta(t, 2, testutil.ToFloat64(metrics.AuditEntryBacklogGauge.WithLabelValues("failed")), 0)
}
