package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

func TestExplainMigrationError_ActionableMessagesKeepTheSentinel(t *testing.T) {
	dirty := explainMigrationError("domain migrations", pgmigrate.ErrMigrationDirty)
	assert.ErrorIs(t, dirty, pgmigrate.ErrMigrationDirty)
	assert.Contains(t, dirty.Error(), "left the schema dirty")

	newer := explainMigrationError("outbox schema", pgmigrate.ErrVersionNotInSource)
	assert.ErrorIs(t, newer, pgmigrate.ErrVersionNotInSource)
	assert.Contains(t, newer.Error(), "newer than this image")

	boom := errors.New("connection refused")
	other := explainMigrationError("domain migrations", boom)
	assert.ErrorIs(t, other, boom)
	assert.Equal(t, "domain migrations: connection refused", other.Error())
}

// An unreachable migration database fails AdoptAuditOutbox (and so
// MigrateSchema) instead of skipping the platform-audit conversion.
func TestAdoptAuditOutbox_UnreachableDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := AdoptAuditOutbox(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1", nil)
	require.Error(t, err)
}
