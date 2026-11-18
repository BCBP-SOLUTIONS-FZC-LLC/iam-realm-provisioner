//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audittest"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestAudit_AdminTool runs the real server binary's `audit-admin` against
// the migrated schema (FinishAdoption done: no compatibility trigger), on
// SYSTEM_DATABASE_URL as realm_provisioner_migrator in PgBouncer mode, as
// production runs it. Two IdP entries Audit Log rejected are parked and a
// TermsAccepted entry whose type it does not know yet waits (skew): `list`
// and `list-waiting` show them; `requeue` replays one parked row and writes
// its own config.audit_delivery.requeued entry; `discard` disposes of the
// other; `discard -waiting` disposes of the waiting row; after
// ReinstallCompat, `resolve -waiting` is refused (ErrAdoptionPending) until
// the next migrate run drops the trigger again.
func TestAudit_AdminTool(t *testing.T) { //nolint:paralleltest // one Runner per process
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "iam-realm-provisioner-server")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/server").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	repo := newAuditRepo(t, appPool)
	tx := pgadapter.NewTxRunner(appPool, nil)

	rejectedA, rejectedB, skewed := uuid.New(), uuid.New(), uuid.New()
	for _, tenantID := range []uuid.UUID{rejectedA, rejectedB} {
		require.NoError(t, tx.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
			return repo.Enqueue(ctx, newIdpAuditEntry(tenantID))
		}))
	}
	terms := domain.TermsAcceptedAuditEntry(skewed, uuid.New(), domain.TermsAcceptance{Version: "2026-09", AcceptedAt: time.Now().UTC()}, "trial", "free", "")
	require.NoError(t, repo.EnqueueOnce(withTenant(ctx, skewed), domain.TermsAcceptedIdempotencyKey(skewed, uuid.New(), "2026-09"), terms))

	fake := audittest.NewServer(t)
	fake.RespondFor(domain.AuditEntryTypeIdpChanged, audittest.Response{Status: 422, Code: "INVALID_ENTRY"})
	fake.RespondFor(domain.AuditEntryTypeTermsAccepted, audittest.Response{Status: 422, Code: "UNKNOWN_ENTRY_TYPE"})
	runner, err := auditoutbox.NewRunner(auditoutbox.Config{
		Pool: sysPool, Client: fake.Client(domain.Source), Enqueuer: newTestEnqueuer(t), PurgeInterval: 0,
	})
	require.NoError(t, err)
	_, err = runner.DrainOnce(ctx)
	require.NoError(t, err)
	require.NoError(t, runner.Stop(ctx))

	rowOf := func(tenantID uuid.UUID) (id, status string) {
		t.Helper()
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT id::text, status FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'source_service' = $2
			 AND entry->>'entry_type' NOT LIKE 'config.audit_delivery.%'`, tenantID, domain.Source).Scan(&id, &status))
		return id, status
	}
	statusOf := func(id string) string {
		t.Helper()
		var st string
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT status FROM pending_audit_entries WHERE id = $1`, id).Scan(&st))
		return st
	}
	parkedA, status := rowOf(rejectedA)
	require.Equal(t, "failed", status)
	parkedB, _ := rowOf(rejectedB)
	waitingID, status := rowOf(skewed)
	require.Equal(t, "pending", status, "skew waits, never parks")

	cfg := rawPool.Config().ConnConfig
	sysDSN := fmt.Sprintf("postgres://realm_provisioner_migrator:%s@%s:%d/%s?sslmode=disable",
		migratorPassword, cfg.Host, cfg.Port, cfg.Database)
	run := func(env []string, wantCode int, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"audit-admin"}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		out, runErr := cmd.CombinedOutput()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code = exitErr.ExitCode()
		} else if runErr != nil {
			t.Fatalf("audit-admin %v: %v", args, runErr)
		}
		require.Equal(t, wantCode, code, "audit-admin %v:\n%s", args, out)
		return string(out)
	}
	appOnly := []string{"DATABASE_URL=" + connStringForApp(t, rawPool), "SYSTEM_DATABASE_URL="}
	sysEnv := []string{"DATABASE_URL=" + connStringForApp(t, rawPool), "SYSTEM_DATABASE_URL=" + sysDSN, "AUDIT_SOURCE_SERVICE="}

	// The RLS-bound app pool is refused before any connection.
	out := run(appOnly, 2, "list")
	assert.Contains(t, out, "SYSTEM_DATABASE_URL is not set")

	out = run(sysEnv, 0, "list")
	assert.Contains(t, out, parkedA)
	assert.Contains(t, out, parkedB)
	assert.Contains(t, out, "422 INVALID_ENTRY")
	assert.Contains(t, out, "2 row(s)")
	out = run(sysEnv, 0, "list-waiting")
	assert.Contains(t, out, waitingID)
	assert.Contains(t, out, "skew")
	assert.Contains(t, out, "1 row(s)")

	operator := uuid.NewString()
	out = run(sysEnv, 0, "requeue", "-id", parkedA, "-operator", operator, "-reason", "fixed upstream")
	assert.Contains(t, out, "requeue: 1 row(s)")
	assert.Equal(t, "pending", statusOf(parkedA))
	countType := func(entryType string) int {
		t.Helper()
		var n int
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT count(*) FROM pending_audit_entries WHERE entry->>'entry_type' = $1`, entryType).Scan(&n))
		return n
	}
	assert.Equal(t, 1, countType("config.audit_delivery.requeued"), "requeue writes its own audit entry")

	out = run(sysEnv, 0, "discard", "-id", parkedB, "-operator", operator, "-reason", "superseded")
	assert.Contains(t, out, "discard: 1 row(s)")
	assert.Equal(t, "disposed", statusOf(parkedB))
	assert.Equal(t, 1, countType("config.audit_delivery.discarded"))

	out = run(sysEnv, 0, "discard", "-waiting", "-id", waitingID, "-operator", operator, "-reason", "type retired")
	assert.Contains(t, out, "discard: 1 row(s)")
	assert.Equal(t, "disposed", statusOf(waitingID))
	assert.Equal(t, 2, countType("config.audit_delivery.discarded"))

	// The library's own refusals: an operator helper's ErrConfig (here
	// -limit 0) is a usage error.
	run(sysEnv, 2, "resolve", "-entry-type", domain.AuditEntryTypeIdpChanged, "-limit", "0", "-operator", operator, "-reason", "x")

	// A database rolled back to pre-library code (ReinstallCompat) has the
	// compatibility trigger again: disposing of waiting rows is refused
	// until the next migrate run's FinishAdoption.
	superDSN := rawPool.Config().ConnString()
	super, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: superDSN, MaxConns: 1})
	require.NoError(t, err)
	defer super.Close()
	require.NoError(t, auditoutbox.ReinstallCompat(ctx, super, auditoutbox.AsBuiltRealmProvisioner))
	out = run(sysEnv, 1, "resolve", "-waiting", "-entry-type", domain.AuditEntryTypeTermsAccepted, "-operator", operator, "-reason", "test")
	assert.Contains(t, out, auditoutbox.ErrAdoptionPending.Error())
	require.NoError(t, pgadapter.AdoptAuditOutbox(ctx, superDSN, nil))
}
