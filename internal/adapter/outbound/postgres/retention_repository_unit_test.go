//go:build !integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPruneProvisioningOperations_ZeroOlderThan_ReturnsError and
// TestPruneProvisioningOperations_NegativeOlderThan_ReturnsError cover the
// pruneLoop guard (olderThan <= 0) that fires before any pool access, so a
// nil pool is safe here.

func TestPruneProvisioningOperations_ZeroOlderThan_ReturnsError(t *testing.T) {
	r := NewRetentionRepository(nil)
	_, err := r.PruneProvisioningOperations(context.Background(), 0, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "olderThan")
}

func TestPruneProvisioningOperations_NegativeOlderThan_ReturnsError(t *testing.T) {
	r := NewRetentionRepository(nil)
	_, err := r.PruneProvisioningOperations(context.Background(), -time.Hour, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "olderThan")
}

// TestPruneRLSViolationLog_ZeroOlderThan_ReturnsError mirrors the integration
// test already in test/postgres/retention_repository_test.go but as a pure
// unit test without a container, covering the same guard path at the
// package level.
func TestPruneRLSViolationLog_ZeroOlderThan_ReturnsError(t *testing.T) {
	r := NewRetentionRepository(nil)
	_, err := r.PruneRLSViolationLog(context.Background(), 0, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "olderThan")
}

func TestPruneRLSViolationLog_NegativeOlderThan_ReturnsError(t *testing.T) {
	r := NewRetentionRepository(nil)
	_, err := r.PruneRLSViolationLog(context.Background(), -24*time.Hour, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "olderThan")
}
