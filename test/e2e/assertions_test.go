//go:build e2e

package e2e_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// assertTenantRealmRow asserts tenant_realms' committed state for tenantID
// via the superuser pool (bypasses RLS by construction — this reads
// downstream Postgres state directly, independent of any GUC a caller did
// or didn't set).
func assertTenantRealmRow(t *testing.T, ctx context.Context, tenantID uuid.UUID, wantStrategy, wantRealm, wantStatus string) {
	t.Helper()
	var strategy, realm, status string
	err := rawPG.QueryRow(ctx, `
		SELECT strategy, realm_name, status FROM tenant_realms WHERE tenant_id = $1 AND deleted_at IS NULL`,
		tenantID).Scan(&strategy, &realm, &status)
	require.NoError(t, err, "tenant_realms row must exist for %s", tenantID)
	require.Equal(t, wantStrategy, strategy, "tenant_realms.strategy")
	require.Equal(t, wantRealm, realm, "tenant_realms.realm_name")
	require.Equal(t, wantStatus, status, "tenant_realms.status")
}

// assertOutboxEvent asserts at least one outbox_events row of eventType was
// enqueued for tenantID (RP-INV-3: enqueued atomically with the business
// write inside the same Postgres transaction — this suite never starts the
// outbox.Runner, so a row here means "the local commit produced this
// event", not "it reached SNS").
func assertOutboxEvent(t *testing.T, ctx context.Context, tenantID uuid.UUID, eventType string) {
	t.Helper()
	var count int
	err := rawPG.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`,
		tenantID.String(), eventType).Scan(&count)
	require.NoError(t, err)
	require.Positive(t, count, "expected an outbox_events row of type %s for tenant %s", eventType, tenantID)
}
