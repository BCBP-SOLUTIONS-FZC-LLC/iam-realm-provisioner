package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"

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
