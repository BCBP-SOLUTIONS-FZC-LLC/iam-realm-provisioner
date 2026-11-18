package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
)

// auditAdminUsage documents `iam-realm-provisioner-server audit-admin`, the
// operator tool for pending_audit_entries rows the platform-audit Runner
// parked (failed) or keeps waiting (skew, not_admitted). It wraps
// platform-audit's audited operator helpers (platform-audit
// docs/guides/operators.md): every requeue, discard and resolve writes its
// own config.audit_delivery.* audit entry in the same transaction,
// attributed to -operator.
const auditAdminUsage = `usage: iam-realm-provisioner-server audit-admin <command> [flags]

Commands:
  list          parked (failed) rows
  list-waiting  rows waiting on skew or not_admitted (never parked)
  requeue       replay parked rows (failed -> pending)
  discard       give up on parked rows, or waiting rows with -waiting
                (kept, scrubbed after AUDIT_DISPOSED_RETENTION)
  resolve       mark parked rows (or waiting rows with -waiting) as
                delivered out of band

Connects with SYSTEM_DATABASE_URL (realm_provisioner_migrator, BYPASSRLS:
the Runner's pool, which sees every tenant's rows), in PgBouncer mode like
the Runner's; refused when SYSTEM_DATABASE_URL is unset or equals
DATABASE_URL (the app role's pool is RLS-filtered). While the as-built
compatibility trigger exists (after auditoutbox.ReinstallCompat, until the
next migrate run's FinishAdoption), discard/resolve -waiting is refused
(ErrAdoptionPending).

requeue/discard/resolve need -operator (your user UUID) and -reason
(1..512 bytes, no personal data: the entry it writes is never redacted),
and at least one selector (-id, -entry-type, -last-outcome or
-failed-before).

Flags:
`

// runAuditAdmin runs one audit-admin command and returns the exit code: 0
// success, 1 operation failed, 2 usage error.
func runAuditAdmin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	stdoutOut, stderrOut := &textOut{w: stdout}, &textOut{w: stderr}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		stderrOut.printf("%s", auditAdminUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("audit-admin "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	ids := fs.String("id", "", "comma-separated pending_audit_entries row ids")
	entryType := fs.String("entry-type", "", "only rows of this entry_type")
	lastOutcome := fs.String("last-outcome", "", "only rows with this last_outcome (rejected, retry, skew, not_admitted, ...)")
	failedBefore := fs.String("failed-before", "", "only rows parked before this RFC 3339 time")
	limit := fs.Int("limit", 100, "maximum rows to list or change")
	operator := fs.String("operator", "", "your user UUID (requeue/discard/resolve)")
	reason := fs.String("reason", "", "why (requeue/discard/resolve; 1..512 bytes, no personal data)")
	waiting := fs.Bool("waiting", false, "discard/resolve: rows waiting on skew or not_admitted instead of parked rows")
	fs.Usage = func() { stderrOut.printf("%s", auditAdminUsage); fs.PrintDefaults() }
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	filter, err := auditAdminFilter(*ids, *entryType, *lastOutcome, *failedBefore)
	if err != nil {
		stderrOut.printf("audit-admin: %v\n", err)
		return 2
	}
	write := cmd == "requeue" || cmd == "discard" || cmd == "resolve"
	if *waiting {
		if cmd != "discard" && cmd != "resolve" {
			stderrOut.printf("%s\n", "audit-admin: -waiting applies to discard and resolve only (list-waiting lists them)")
			return 2
		}
		if !filter.FailedBefore.IsZero() {
			stderrOut.printf("%s\n", "audit-admin: -failed-before applies to parked rows only")
			return 2
		}
		filter.Waiting = true
	}
	switch {
	case cmd == "list" || cmd == "list-waiting" || write:
	default:
		stderrOut.printf("audit-admin: unknown command %q\n\n%s", cmd, auditAdminUsage)
		return 2
	}
	var op audit.Actor
	var enq *auditoutbox.Enqueuer
	if write {
		opID, parseErr := uuid.Parse(*operator)
		if parseErr != nil || opID == uuid.Nil {
			stderrOut.printf("%s\n", "audit-admin: -operator must be your user UUID")
			return 2
		}
		if *reason == "" {
			stderrOut.printf("%s\n", "audit-admin: -reason is required")
			return 2
		}
		if filter.IDs == nil && filter.EntryType == "" && filter.LastOutcome == "" && filter.FailedBefore.IsZero() {
			stderrOut.printf("%s\n", "audit-admin: refusing to change every parked row: pass -id, -entry-type, -last-outcome or -failed-before")
			return 2
		}
		op = audit.Actor{Type: audit.ActorUser, ID: opID.String()}

		// The server's Enqueuer configuration (loadAuditConfig: the same
		// AUDIT_* environment and AUDIT_SOURCE_SERVICE check), without
		// metrics or a Runner.
		auditCfg, cfgErr := loadAuditConfig()
		if cfgErr != nil {
			stderrOut.printf("audit-admin: %v\n", cfgErr)
			return 2
		}
		if enq, err = auditoutbox.NewEnqueuer(auditCfg.EnqueueConfig(nil)); err != nil {
			stderrOut.printf("audit-admin: enqueuer: %v\n", err)
			return 1
		}
	}

	// The Runner's pool: the BYPASSRLS sysPool. Without SYSTEM_DATABASE_URL
	// SystemDSNFromEnv falls back to the RLS-bound app DSN, which sees no
	// tenant's rows without a tenant GUC; the Runner refuses that pool too.
	sysDSN := pgadapter.SystemDSNFromEnv()
	if sysDSN == "" || sysDSN == pgadapter.DSNFromEnv() {
		stderrOut.printf("%s\n", "audit-admin: SYSTEM_DATABASE_URL is not set or equals DATABASE_URL: the tool needs the Runner's BYPASSRLS pool (realm_provisioner_migrator); the app role's pool is RLS-filtered and cannot see every tenant's rows")
		return 2
	}
	pgCfg := pgadapter.SystemPoolConfig(sysDSN, nil)
	pgCfg.MaxConns, pgCfg.MinConns, pgCfg.PoolName = 1, 0, "audit_admin"
	pool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		stderrOut.printf("audit-admin: connect: %v\n", err)
		return 1
	}
	defer pool.Close()

	if !write {
		var rows []auditoutbox.FailedRow
		if cmd == "list" {
			rows, err = auditoutbox.ListFailed(ctx, pool, filter, *limit)
		} else {
			rows, err = auditoutbox.ListWaiting(ctx, pool, filter, *limit)
		}
		if err != nil {
			stderrOut.printf("audit-admin: %s: %v\n", cmd, err)
			return 1
		}
		printAuditRows(stdout, rows)
		return 0
	}

	var n int
	switch cmd {
	case "requeue":
		n, err = auditoutbox.Requeue(ctx, pool, enq, op, filter, *limit, *reason)
	case "discard":
		n, err = auditoutbox.Discard(ctx, pool, enq, op, filter, *limit, *reason)
	case "resolve":
		n, err = auditoutbox.Resolve(ctx, pool, enq, op, filter, *limit, *reason)
	}
	if err != nil {
		stderrOut.printf("audit-admin: %s: %v\n", cmd, err)
		if errors.Is(err, audit.ErrConfig) || errors.Is(err, audit.ErrInvalidEntry) {
			return 2
		}
		return 1
	}
	stdoutOut.printf("%s: %d row(s)\n", cmd, n)
	return 0
}

// auditAdminFilter builds the operator helpers' Filter from the flags.
func auditAdminFilter(ids, entryType, lastOutcome, failedBefore string) (auditoutbox.Filter, error) {
	f := auditoutbox.Filter{EntryType: entryType, LastOutcome: lastOutcome}
	for _, s := range strings.Split(ids, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return f, fmt.Errorf("-id %q is not a UUID", s)
		}
		f.IDs = append(f.IDs, id)
	}
	if failedBefore != "" {
		t, err := time.Parse(time.RFC3339, failedBefore)
		if err != nil {
			return f, fmt.Errorf("-failed-before must be RFC 3339: %w", err)
		}
		f.FailedBefore = t
	}
	return f, nil
}

// printAuditRows prints rows without entry content: the helpers never return
// any beyond the entry type.
func printAuditRows(w io.Writer, rows []auditoutbox.FailedRow) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	twOut, wOut := &textOut{w: tw}, &textOut{w: w}
	twOut.printf("%s\n", "ID\tTENANT\tENTRY TYPE\tLAST OUTCOME\tATTEMPTS\tFAILED AT\tLAST ERROR")
	for _, r := range rows {
		failedAt := "-"
		if r.FailedAt != nil {
			failedAt = r.FailedAt.UTC().Format(time.RFC3339)
		}
		twOut.printf("%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.ID, r.TenantID, r.EntryType, r.LastOutcome, r.Attempts, failedAt, r.LastError)
	}
	if err := tw.Flush(); err != nil {
		return // the terminal is gone; nothing left to tell the operator
	}
	wOut.printf("%d row(s)\n", len(rows))
}

// textOut writes operator output, keeping the first write error: a CLI
// cannot report a broken stdout/stderr anywhere else, and the exit code
// already says how the command went.
type textOut struct {
	w   io.Writer
	err error
}

func (o *textOut) printf(format string, a ...any) {
	if o.err == nil {
		_, o.err = fmt.Fprintf(o.w, format, a...)
	}
}
