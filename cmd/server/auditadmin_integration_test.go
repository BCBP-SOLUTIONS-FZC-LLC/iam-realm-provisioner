//go:build integration

// runAuditAdmin in-process against a real migrated database (so the
// operation paths count toward coverage); test/postgres's TestAudit_AdminTool
// runs the built binary end to end.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audittest"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
)

func TestRunAuditAdmin_RealSchema(t *testing.T) {
	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16.16-alpine",
		tcpostgres.WithDatabase("provisioner"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })
	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, pgadapter.MigrateSchema(ctx, dsn, nil))

	// The superuser stands in for realm_provisioner_migrator (it sees every
	// tenant's rows); the app DSN only has to differ from it.
	t.Setenv("SYSTEM_DATABASE_URL", dsn)
	t.Setenv("DATABASE_URL", strings.Replace(dsn, "postgres:testpassword@", "realm_provisioner_app:pw@", 1))
	t.Setenv("AUDIT_SOURCE_SERVICE", "")

	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn, GUCProvider: pgcommon.GUCSetFromContext})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	raw, err := dbseed.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	enq, err := auditoutbox.NewEnqueuer(auditoutbox.EnqueueConfig{SourceService: domain.Source, Strict: true})
	require.NoError(t, err)
	repo := pgadapter.NewPendingAuditEntryRepository(pool, enq)
	txr := pgadapter.NewTxRunner(pool, nil)
	enqueue := func(tenantID uuid.UUID) {
		t.Helper()
		g, _ := pgcommon.GUCSetFromContext(ctx)
		g.TenantID = tenantID.String()
		require.NoError(t, txr.RunInTx(pgcommon.WithGUCSet(ctx, g), func(ctx context.Context) error {
			return repo.Enqueue(ctx, domain.AuditEntry{
				TenantID: tenantID, EntryType: domain.AuditEntryTypeIdpChanged, Action: domain.AuditActionCreate,
				Actor:      domain.AuditActor{Type: domain.AuditActorIAMSystem, ID: domain.SystemActorID},
				Target:     &domain.AuditTarget{Type: "idp", ID: "acme-sso"},
				OccurredAt: time.Now().UTC().Truncate(time.Millisecond), SourceService: domain.Source,
				SourceEventType: domain.AuditSourceTenantIdpConfigChanged,
				Metadata:        json.RawMessage(`{"validation_outcome":"pass"}`),
			})
		}))
	}
	parked, waiting := uuid.New(), uuid.New()
	enqueue(parked)
	enqueue(waiting)

	// Park the first row (422 INVALID_ENTRY), then keep the second waiting
	// (422 UNKNOWN_ENTRY_TYPE).
	fake := audittest.NewServer(t)
	drain := func(r audittest.Response) {
		t.Helper()
		fake.RespondFor(domain.AuditEntryTypeIdpChanged, r)
		runner, err := auditoutbox.NewRunner(auditoutbox.Config{Pool: pool, Client: fake.Client(domain.Source), Enqueuer: enq})
		require.NoError(t, err)
		_, err = runner.DrainOnce(ctx)
		require.NoError(t, err)
		require.NoError(t, runner.Stop(ctx))
	}
	idOf := func(tenantID uuid.UUID) string {
		t.Helper()
		var id string
		require.NoError(t, raw.QueryRow(ctx, `SELECT id::text FROM pending_audit_entries WHERE tenant_id = $1`, tenantID).Scan(&id))
		return id
	}
	_, err = raw.Exec(ctx, `UPDATE pending_audit_entries SET next_attempt_at = 'infinity' WHERE tenant_id = $1`, waiting)
	require.NoError(t, err)
	drain(audittest.Response{Status: 422, Code: "INVALID_ENTRY"})
	_, err = raw.Exec(ctx, `UPDATE pending_audit_entries SET next_attempt_at = now() WHERE tenant_id = $1`, waiting)
	require.NoError(t, err)
	drain(audittest.Response{Status: 422, Code: "UNKNOWN_ENTRY_TYPE"})
	parkedID, waitingID := idOf(parked), idOf(waiting)

	run := func(want int, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runAuditAdmin(ctx, args, &stdout, &stderr)
		require.Equal(t, want, code, "audit-admin %v\nstdout: %s\nstderr: %s", args, stdout.String(), stderr.String())
		return stdout.String() + stderr.String()
	}
	op := uuid.NewString()

	assert.Contains(t, run(0, "list"), parkedID)
	assert.Contains(t, run(0, "list-waiting"), waitingID)
	assert.Contains(t, run(1, "list", "-limit", "0"), "audit-admin: list:", "the library's limit check fails the operation")
	assert.Contains(t, run(2, "requeue", "-id", parkedID, "-limit", "0", "-operator", op, "-reason", "x"), "audit-admin: requeue:")
	assert.Contains(t, run(0, "requeue", "-id", parkedID, "-operator", op, "-reason", "fixed upstream"), "requeue: 1 row(s)")
	assert.Contains(t, run(0, "resolve", "-waiting", "-id", waitingID, "-operator", op, "-reason", "sent out of band"), "resolve: 1 row(s)")

	drain(audittest.Response{Status: 422, Code: "INVALID_ENTRY"}) // parks the replayed row again
	assert.Contains(t, run(0, "discard", "-id", parkedID, "-operator", op, "-reason", "superseded"), "discard: 1 row(s)")
	var statuses []string
	rows, err := raw.Query(ctx, `SELECT status FROM pending_audit_entries WHERE id = ANY($1::uuid[]) ORDER BY status`, []string{parkedID, waitingID})
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		statuses = append(statuses, s)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"disposed", "disposed"}, statuses)

	// The sysPool connects in PgBouncer mode with its own name; an
	// unreachable one is an operation failure.
	t.Setenv("SYSTEM_DATABASE_URL", strings.Replace(dsn, "testpassword", "wrong", 1))
	assert.Contains(t, run(1, "list"), "audit-admin: connect:")
}
