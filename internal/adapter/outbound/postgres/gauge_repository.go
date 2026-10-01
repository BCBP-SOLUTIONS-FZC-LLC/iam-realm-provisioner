package postgres

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// GaugeRepository serves the read-only, cross-tenant aggregate queries that
// back the DB-state gauges in §11.2 (realm_active_gauge,
// realm_config_pending_gauge, template_drift). Those gauges are counts of
// current table state rather than event tallies, so nothing on a request or
// reconciler path can maintain them; cmd/server polls this repository on an
// interval instead.
//
// It MUST be constructed with the BYPASSRLS sysPool (§10.1) — every query
// here spans tenants and would return zero rows through the RLS-scoped app
// pool with no tenant GUC bound.
//
// Unlike the other repositories in this package it implements no core port:
// its only consumer is the composition root's exporter goroutines, so
// routing it through the domain would add an interface no service uses.
type GaugeRepository struct {
	pool *pgcommon.Pool
}

// NewGaugeRepository constructs a GaugeRepository over the BYPASSRLS pool.
func NewGaugeRepository(sysPool *pgcommon.Pool) *GaugeRepository {
	return &GaugeRepository{pool: sysPool}
}

// activeRealmsByStrategySQL counts active realms per strategy.
//
// The LEFT JOIN over enum_range is what keeps the gauge honest: a plain
// GROUP BY emits no row for a strategy with zero active realms, so the
// previous poll's non-zero value would linger on that label indefinitely.
// Joining against the enum guarantees exactly one row per strategy,
// including the zeros, without hard-coding the enum members in Go.
const activeRealmsByStrategySQL = `
SELECT s.strategy::text, count(r.tenant_id)
  FROM unnest(enum_range(NULL::realm_strategy)) AS s(strategy)
  LEFT JOIN public.tenant_realms r
    ON r.strategy = s.strategy
   AND r.status = 'active'
   AND r.deleted_at IS NULL
 GROUP BY s.strategy`

// ActiveRealmsByStrategy returns the count of active, non-deleted realms
// keyed by strategy. Every strategy in the realm_strategy enum is present in
// the result, mapped to 0 when it has none.
func (r *GaugeRepository) ActiveRealmsByStrategy(ctx context.Context) (map[string]int64, error) {
	return r.groupedCounts(ctx, activeRealmsByStrategySQL)
}

// pendingRealmConfigsSQL backs the §11.6 realm_config_pending_gauge > 0
// alert, which could not fire at all while nothing populated the gauge.
const pendingRealmConfigsSQL = `
SELECT count(*) FROM public.realm_config_reconciliations WHERE status = 'pending'`

// PendingRealmConfigs returns the number of realm-config reconciliation rows
// still awaiting application (RP-C4).
func (r *GaugeRepository) PendingRealmConfigs(ctx context.Context) (int64, error) {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, pendingRealmConfigsSQL).Scan(&n)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// templateDriftByVersionSQL counts dedicated realms whose template version
// differs from the version this deployment ships, grouped by the version
// they are stuck on.
//
// IS DISTINCT FROM (not <>) so realms with a NULL version — never stamped
// with one — count as drifted instead of dropping out of the comparison
// entirely; they surface under the 'unset' label.
//
// The status filter excludes realms that cannot meaningfully be
// re-templated: 'provisioning' is mid-creation and would flap the gauge
// during every signup, and 'exported'/'deleted' are terminal. 'suspended'
// and 'disabled' are deliberately included — a lapsed tenant sitting on an
// old template is real drift that must be fixed before reactivation.
const templateDriftByVersionSQL = `
SELECT coalesce(realm_template_version, 'unset'), count(*)
  FROM public.tenant_realms
 WHERE strategy = 'dedicated'
   AND deleted_at IS NULL
   AND status NOT IN ('provisioning', 'exported', 'deleted')
   AND realm_template_version IS DISTINCT FROM $1
 GROUP BY 1`

// TemplateDriftByVersion returns the count of dedicated realms drifted from
// currentVersion, keyed by the realm_template_version they are on ('unset'
// for realms that never had one stamped). Versions with no drifted realms
// are absent from the map rather than present as 0, so callers can drop
// their stale series.
func (r *GaugeRepository) TemplateDriftByVersion(ctx context.Context, currentVersion string) (map[string]int64, error) {
	return r.groupedCounts(ctx, templateDriftByVersionSQL, currentVersion)
}

// rlsViolationsWatermarkName identifies this exporter's row in
// exporter_watermarks (§4.2). One name, one row — this repository backs
// exactly one watermark-based exporter today.
const rlsViolationsWatermarkName = "rls_violations_total"

// RLSViolationCounts returns the count of rls_violation_log rows recorded
// since this exporter's persisted high-water mark, keyed by violation_type,
// and advances that mark to the query's own snapshot time. RLS-6's audit
// trail (§17.5 Case 5) is sampled at 1% by log_rls_violation() — this is an
// under-count of true violation volume, not an exact tally, but sufficient
// to detect that violations are occurring at all.
//
// A persisted mark rather than a fixed trailing window is what makes this
// safe under both a pod restart (the new pod picks up exactly where the
// prior one left off, instead of re-scraping a fixed window and re-adding
// counts the dying pod already tallied onto the Prometheus counter) and
// multiple concurrent server replicas (HPA minReplicas: 2 — every replica
// runs this same exporter goroutine against the same table). The `SELECT
// ... FOR UPDATE` below serializes concurrent callers on the watermark
// row: a second replica's tick blocks until the first commits its advanced
// mark, then queries a window that's already been drained to ~empty,
// rather than every replica redundantly re-counting the same rows on every
// tick. initialWindow seeds the mark on this exporter's first-ever run (no
// row yet) so a brand-new deployment doesn't try to scrape the table's
// entire history in one go.
//
// DEPLOYMENT PREREQUISITE: this runs on sysPool, i.e. as
// realm_provisioner_migrator. Unlike the other GaugeRepository queries, the
// domain migration grants this role nothing on rls_violation_log OR
// exporter_watermarks — every one of its privileges on either table is
// provisioned by Terraform outside this repo (§10.1). Until that Terraform
// is updated to also `GRANT SELECT ON rls_violation_log` and `GRANT
// SELECT, INSERT, UPDATE ON exporter_watermarks TO
// realm_provisioner_migrator`, this query — and both RLS-6 alerts that
// depend on it — will fail with a permission-denied error in any real
// environment (test/postgres/syspool_helper_test.go grants both directly,
// since the test harness doesn't run the real Terraform either).
func (r *GaugeRepository) RLSViolationCounts(ctx context.Context, initialWindow time.Duration) (map[string]int64, error) {
	out := map[string]int64{}
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO exporter_watermarks (exporter_name, last_processed_at)
			VALUES ($1, now() - $2::interval)
			ON CONFLICT (exporter_name) DO NOTHING`,
			rlsViolationsWatermarkName, initialWindow.String()); err != nil {
			return err
		}

		var from time.Time
		if err := tx.QueryRow(ctx, `
			SELECT last_processed_at FROM exporter_watermarks
			 WHERE exporter_name = $1 FOR UPDATE`,
			rlsViolationsWatermarkName).Scan(&from); err != nil {
			return err
		}

		to := time.Now().UTC()
		rows, err := tx.Query(ctx, `
			SELECT violation_type, count(*)
			  FROM public.rls_violation_log
			 WHERE occurred_at > $1 AND occurred_at <= $2
			 GROUP BY violation_type`, from, to)
		if err != nil {
			return err
		}
		for rows.Next() {
			var label string
			var n int64
			if scanErr := rows.Scan(&label, &n); scanErr != nil {
				rows.Close()
				return scanErr
			}
			out[label] = n
		}
		if err := rows.Err(); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE exporter_watermarks SET last_processed_at = $2
			 WHERE exporter_name = $1`, rlsViolationsWatermarkName, to)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// groupedCounts runs a query returning (text label, bigint count) rows and
// collects them into a map. The map is built before it is returned so a
// mid-scan failure surfaces as an error rather than a partial result the
// caller might publish.
func (r *GaugeRepository) groupedCounts(ctx context.Context, sql string, args ...any) (map[string]int64, error) {
	out := map[string]int64{}
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				label string
				count int64
			)
			if err := rows.Scan(&label, &count); err != nil {
				return err
			}
			out[label] = count
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// auditEntryBacklogSQL counts pending_audit_entries rows per status. The
// VALUES join keeps both statuses present at 0 rather than dropping a label.
const auditEntryBacklogSQL = `
SELECT s.status, count(p.id)
FROM (VALUES ('pending'), ('failed')) AS s(status)
LEFT JOIN public.pending_audit_entries p ON p.status = s.status
GROUP BY s.status`

// AuditEntryBacklog returns the pending_audit_entries row count by status
// (TenantIdpConfigChanged AL-5 hand-off, §10.6).
func (r *GaugeRepository) AuditEntryBacklog(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, auditEntryBacklogSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var status string
			var n int64
			if err := rows.Scan(&status, &n); err != nil {
				return err
			}
			out[status] = n
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
