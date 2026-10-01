// AL-5 audit-entry drainer started from main.go (§10.6). IdpService
// enqueues each TenantIdpConfigChanged entry into pending_audit_entries in
// the same transaction as the IdP-config write it records; this goroutine
// delivers them to Audit Log's direct-write ingest. It runs in the server
// pod (like the metric exporters), polling the BYPASSRLS sysPool since the
// backlog is cross-tenant; concurrent replicas are safe because ClaimDue
// leases rows with FOR UPDATE SKIP LOCKED.
package main

import (
	"context"
	"os"
	"time"

	auditlogclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/auditlog"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// auditDrainer is the one AuditDeliveryService method the loop needs.
type auditDrainer interface {
	DrainOnce(ctx context.Context, limit int) (int, error)
}

// startAuditDelivery starts the drainer unless AUDIT_LOG_BASE_URL is
// unset. Unset is not fatal — the Audit Log Service may not be deployed
// yet in every environment — and loses nothing: entries stay durably in
// pending_audit_entries (visible on audit_entry_backlog_gauge) and are
// delivered once the URL is configured.
func startAuditDelivery(ctx context.Context, sysPool *pgcommon.Pool, log port.Logger) {
	baseURL := os.Getenv("AUDIT_LOG_BASE_URL")
	if baseURL == "" {
		log.Warn("audit-entry delivery disabled — AUDIT_LOG_BASE_URL unset; TenantIdpConfigChanged entries accumulate in pending_audit_entries until it is set", nil)
		return
	}
	svc := service.NewAuditDeliveryService(
		pgadapter.NewPendingAuditEntryRepository(sysPool),
		auditlogclient.New(baseURL, log),
	).WithLogger(log)
	go runAuditDelivery(ctx, svc,
		envDuration("AUDIT_DELIVERY_INTERVAL", 5*time.Second),
		envInt("AUDIT_DELIVERY_BATCH_SIZE", 50), log)
	log.Info("audit-entry delivery wired", map[string]any{"audit_log_base_url": baseURL})
}

// runAuditDelivery drains on every interval tick until ctx is canceled. A
// full batch is followed immediately by another drain, so a backlog built
// up during an Audit Log outage clears without waiting a tick per batch.
func runAuditDelivery(ctx context.Context, d auditDrainer, interval time.Duration, batch int, log port.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil {
			n, err := d.DrainOnce(ctx, batch)
			if err != nil {
				log.Warn("audit-entry drain failed", map[string]any{"error": err.Error()})
				break
			}
			if n < batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
