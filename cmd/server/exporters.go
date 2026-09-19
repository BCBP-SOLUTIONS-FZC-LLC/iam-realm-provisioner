// Metric exporter goroutines started from main.go (§11.2). Each polls the
// BYPASSRLS sysPool on an interval and refreshes one DB-state gauge.
// Follows the sibling iam-org-membership cmd/server/exporters.go pattern:
// exporters run as goroutines in the server pod, not as CronJobs, so the pod
// that serves /metrics is also the one that populates them. The SQL itself
// lives in the postgres adapter (pgadapter.GaugeRepository) per this repo's
// rule that no SQL is written outside internal/adapter/outbound/postgres.
package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// exporterInterval is a var, not a const, purely so a test can shrink it
// for the duration of one case (t.Cleanup restores it) to make tick's
// periodic re-fire branch exercisable in well under 5 minutes — production
// behavior is unaffected, nothing else in this file ever reassigns it.
var exporterInterval = 5 * time.Minute

// runBusinessExporters starts the goroutines that keep the three DB-state
// gauges in §11.2, plus the rls_violations_total counter (RLS-6, §17.5 Case
// 5), fresh. Before the gauges existed they were registered but permanently
// empty, which also meant the §11.6 realm_config_pending_gauge alert could
// never fire; rls_violations_total had the same problem — registered,
// never incremented, so its two alerts could never fire either.
//
// Each gauge exporter emits once at start so it is populated before the
// first Prometheus scrape, then ticks every exporterInterval until ctx is
// canceled at shutdown. Query failures are logged at WARN and the previous
// value is retained — a stale gauge beats a crashed pod. The counter
// exporter has no equivalent "populate before first scrape" concern (a
// counter starting at 0 is correct, not stale).
func runBusinessExporters(ctx context.Context, gauges *pgadapter.GaugeRepository, log port.Logger) {
	templateVersion := envOr("REALM_TEMPLATE_VERSION", "v1")

	go tick(ctx, "realm_config_pending_gauge", log, func() error {
		n, err := gauges.PendingRealmConfigs(ctx)
		if err != nil {
			return err
		}
		metrics.RealmConfigPendingGauge.Set(float64(n))
		return nil
	})

	// No Reset: the label set is the fixed realm_strategy enum and the query
	// returns a row per member, so a strategy with no realms must report 0
	// rather than disappear.
	go tick(ctx, "realm_active_gauge", log, func() error {
		counts, err := gauges.ActiveRealmsByStrategy(ctx)
		if err != nil {
			return err
		}
		setGaugeVec(metrics.RealmActiveGauge, counts, false)
		return nil
	})

	// Reset: the drifting-version label set shrinks as realms are upgraded,
	// and a version that no longer drifts must stop being reported instead
	// of sitting at its last non-zero count forever.
	go tick(ctx, "template_drift", log, func() error {
		counts, err := gauges.TemplateDriftByVersion(ctx, templateVersion)
		if err != nil {
			return err
		}
		setGaugeVec(metrics.TemplateDrift, counts, true)
		return nil
	})

	// rls_violations_total is monotonic (Add, never Set/Reset) — each tick
	// adds the count RLSViolationCounts observed since its own persisted
	// high-water mark (not a fixed trailing window), so the counter's total
	// only ever grows and alerts use rate()/increase() over a window rather
	// than reading the counter's absolute value. The persisted mark — not
	// exporterInterval — is what makes this correct across a pod restart
	// and across the multiple concurrent server replicas HPA runs: every
	// replica shares the same mark and serializes on it (see
	// RLSViolationCounts), so neither a restart nor another replica's tick
	// ever re-adds a violation this or another pod already counted.
	// exporterInterval here only seeds the mark on this exporter's
	// first-ever run. A violation_type absent from this tick's result
	// contributes 0, not a reset to 0 — an unrelated label must never lose
	// its history.
	go tick(ctx, "rls_violations_total", log, func() error {
		counts, err := gauges.RLSViolationCounts(ctx, exporterInterval)
		if err != nil {
			return err
		}
		for violationType, n := range counts {
			if n > 0 {
				metrics.RLSViolationsTotal.WithLabelValues(violationType).Add(float64(n))
				metrics.RLSViolationsPlatformTotal.WithLabelValues(violationType).Add(float64(n))
			}
		}
		return nil
	})
}

// setGaugeVec publishes counts onto a single-label GaugeVec. When reset is
// true, labels absent from counts are dropped rather than left at their
// previous value.
func setGaugeVec(vec *prometheus.GaugeVec, counts map[string]int64, reset bool) {
	if reset {
		vec.Reset()
	}
	for label, n := range counts {
		vec.WithLabelValues(label).Set(float64(n))
	}
}

// tick runs emit once, then on exporterInterval until ctx is canceled.
func tick(ctx context.Context, gauge string, log port.Logger, emit func() error) {
	run := func() {
		if err := emit(); err != nil {
			log.Warn("gauge exporter query failed", map[string]any{
				"gauge": gauge, "error": err.Error(),
			})
		}
	}
	run() // populate before the first scrape
	ticker := time.NewTicker(exporterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
