package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
)

// The pool's DSN env for these tests: no DSN at all unless a case sets one.
func clearAuditAdminDSNs(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SYSTEM_DATABASE_URL", "")
}

func TestRunAuditAdmin_UsageErrors(t *testing.T) {
	op := uuid.NewString()
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no command":                 {nil, "usage: iam-realm-provisioner-server audit-admin"},
		"help":                       {[]string{"-h"}, "list-waiting"},
		"unknown command":            {[]string{"purge"}, `unknown command "purge"`},
		"unknown flag":               {[]string{"list", "-tenant", "x"}, "flag provided but not defined"},
		"bad id":                     {[]string{"list", "-id", "nope"}, `-id "nope" is not a UUID`},
		"bad failed-before":          {[]string{"list", "-failed-before", "yesterday"}, "-failed-before must be RFC 3339"},
		"missing operator":           {[]string{"requeue", "-reason", "x", "-entry-type", "t"}, "-operator must be your user UUID"},
		"nil operator":               {[]string{"discard", "-operator", uuid.Nil.String(), "-reason", "x", "-entry-type", "t"}, "-operator must be your user UUID"},
		"missing reason":             {[]string{"resolve", "-operator", op, "-entry-type", "t"}, "-reason is required"},
		"no selector":                {[]string{"requeue", "-operator", op, "-reason", "x"}, "refusing to change every parked row"},
		"no system DSN":              {[]string{"list"}, "SYSTEM_DATABASE_URL is not set"},
		"waiting on requeue":         {[]string{"requeue", "-waiting", "-operator", op, "-reason", "x", "-entry-type", "t"}, "-waiting applies to discard and resolve only"},
		"waiting on list":            {[]string{"list", "-waiting"}, "-waiting applies to discard and resolve only"},
		"waiting with failed-before": {[]string{"discard", "-waiting", "-failed-before", "2026-11-01T00:00:00Z", "-operator", op, "-reason", "x"}, "-failed-before applies to parked rows only"},
		"no system DSN for a change": {[]string{"requeue", "-operator", op, "-reason", "x", "-id", uuid.NewString()}, "SYSTEM_DATABASE_URL is not set"},
	} {
		t.Run(name, func(t *testing.T) {
			clearAuditAdminDSNs(t)
			var stdout, stderr bytes.Buffer
			if code := runAuditAdmin(context.Background(), tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code %d, want 2 (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), tc.want)
			}
		})
	}
}

// The tool refuses the RLS-bound app pool: SYSTEM_DATABASE_URL equal to
// DATABASE_URL is what SystemDSNFromEnv's fallback would produce.
func TestRunAuditAdmin_RefusesTheAppPool(t *testing.T) {
	const dsn = "postgres://realm_provisioner_app:pw@127.0.0.1:1/provisioner?sslmode=disable"
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SYSTEM_DATABASE_URL", dsn)
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 2, runAuditAdmin(context.Background(), []string{"list"}, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "RLS-filtered")
}

// A change loads the server's AUDIT_* configuration first: a malformed value
// or a foreign AUDIT_SOURCE_SERVICE is a usage error, before any connection.
func TestRunAuditAdmin_AuditConfigErrors(t *testing.T) {
	op := uuid.NewString()
	args := []string{"requeue", "-operator", op, "-reason", "x", "-id", uuid.NewString()}
	for name, tc := range map[string]struct{ env, value, want string }{
		"foreign source service": {auditconfig.EnvSourceService, "iam-catalog-admin", "AUDIT_SOURCE_SERVICE must be"},
		"malformed timeout":      {"AUDIT_LOG_TIMEOUT", "soon", "AUDIT_LOG_TIMEOUT"},
	} {
		t.Run(name, func(t *testing.T) {
			clearAuditAdminDSNs(t)
			t.Setenv(tc.env, tc.value)
			var stdout, stderr bytes.Buffer
			assert.Equal(t, 2, runAuditAdmin(context.Background(), args, &stdout, &stderr))
			assert.Contains(t, stderr.String(), tc.want)
			assert.NotContains(t, stderr.String(), "SYSTEM_DATABASE_URL", "checked before any connection")
		})
	}
}

// An unreachable database is an operation failure (exit 1), not a usage
// error.
func TestRunAuditAdmin_UnreachableDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://realm_provisioner_app:pw@127.0.0.1:1/provisioner?sslmode=disable&connect_timeout=2")
	t.Setenv("SYSTEM_DATABASE_URL", "postgres://realm_provisioner_migrator:pw@127.0.0.1:1/provisioner?sslmode=disable&connect_timeout=2")
	op := uuid.NewString()
	for _, args := range [][]string{
		{"list"},
		{"requeue", "-operator", op, "-reason", "x", "-id", uuid.NewString()},
	} {
		t.Run(args[0], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			assert.Equal(t, 1, runAuditAdmin(ctx, args, &stdout, &stderr), stderr.String())
			assert.Contains(t, stderr.String(), "audit-admin: connect:")
		})
	}
}

func TestAuditAdminFilter(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	f, err := auditAdminFilter(a.String()+", "+b.String(), "config.plan.updated", "rejected", "2026-11-01T00:00:00Z")
	require.NoError(t, err)
	require.Len(t, f.IDs, 2)
	assert.Equal(t, a, f.IDs[0])
	assert.Equal(t, b, f.IDs[1])
	assert.Equal(t, "config.plan.updated", f.EntryType)
	assert.Equal(t, "rejected", f.LastOutcome)
	assert.False(t, f.FailedBefore.IsZero())

	empty, err := auditAdminFilter("", "", "", "")
	require.NoError(t, err)
	assert.Nil(t, empty.IDs, "no -id is no id selector")
}

func TestPrintAuditRows(t *testing.T) {
	failedAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	id := uuid.New()
	var out bytes.Buffer
	printAuditRows(&out, []auditoutbox.FailedRow{
		{ID: id, TenantID: uuid.New(), EntryType: "legal.terms.accepted", LastOutcome: "rejected", LastError: "422 INVALID_ENTRY", Attempts: 1, FailedAt: &failedAt},
		{ID: uuid.New(), TenantID: uuid.New(), EntryType: "tenant.idp_config.changed", LastOutcome: "skew", Attempts: 3},
	})
	s := out.String()
	assert.Contains(t, s, "ID")
	assert.Contains(t, s, id.String())
	assert.Contains(t, s, "2026-11-01T10:00:00Z")
	assert.Contains(t, s, "422 INVALID_ENTRY")
	assert.Contains(t, s, "2 row(s)")
}

// A broken stdout keeps the first write error and writes nothing more.
func TestPrintAuditRows_BrokenWriter(t *testing.T) {
	w := &failingWriter{}
	printAuditRows(w, []auditoutbox.FailedRow{{ID: uuid.New()}})
	assert.Equal(t, 1, w.calls, "the failed flush stops the row count line")

	o := &textOut{w: w}
	o.printf("a")
	o.printf("b")
	assert.Equal(t, 2, w.calls, "textOut stops after its first error")
}

type failingWriter struct{ calls int }

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("broken pipe")
}
