//go:build integration

// Package postgres_test covers RunMigrations (migrate.go): the optional
// variadic Logger parameter, and idempotent re-application against an
// already-migrated database. Every other test in this package calls
// setupTestDB, which itself calls pgadapter.RunMigrations against a fresh
// database with no logger — exercising only "fresh database, no logger".
// This file covers the two remaining combinations.
package postgres_test

import (
	"context"
	"fmt"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunMigrations_AlreadyMigratedDatabaseWithLogger_IsIdempotent(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t) // already ran RunMigrations once, with no logger
	ctx := context.Background()

	cfg := rawPool.Config().ConnConfig
	superDSN := fmt.Sprintf("postgres://postgres:testpassword@%s:%d/%s?sslmode=disable",
		cfg.Host, cfg.Port, cfg.Database)

	fake := &fakePortLogger{}
	err := pgadapter.RunMigrations(ctx, superDSN, fake)
	require.NoError(t, err, "re-running RunMigrations against an already-migrated database must be a no-op, not an error")

	var foundNoChangeLog bool
	for _, call := range fake.calls {
		if call.level == "info" {
			foundNoChangeLog = true
		}
	}
	assert.True(t, foundNoChangeLog, "the supplied port.Logger must receive at least one Info call from the underlying pgmigrate.Runner")
}

func TestRunMigrations_NilLoggerVariadic_DoesNotPanic(t *testing.T) {
	t.Parallel()
	_, rawPool := setupTestDB(t)
	ctx := context.Background()

	cfg := rawPool.Config().ConnConfig
	superDSN := fmt.Sprintf("postgres://postgres:testpassword@%s:%d/%s?sslmode=disable",
		cfg.Host, cfg.Port, cfg.Database)

	// A nil port.Logger passed explicitly must be treated the same as
	// omitting the variadic argument entirely (log[0] != nil guard).
	assert.NotPanics(t, func() {
		err := pgadapter.RunMigrations(ctx, superDSN, nil)
		assert.NoError(t, err)
	})
}
