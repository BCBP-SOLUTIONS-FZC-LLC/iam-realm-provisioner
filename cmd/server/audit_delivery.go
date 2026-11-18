// Audit Log AL-5 delivery through platform-audit's outbox (§10.6,
// platform-audit iam-adopters §5.1). IdpService (TenantIdpConfigChanged)
// and ProvisioningService (TermsAccepted) enqueue into pending_audit_entries
// in the same transaction as the write each entry records; the library's
// Runner delivers them from the server pod, on the BYPASSRLS sysPool since
// the backlog is cross-tenant. Concurrent replicas are safe: the Runner
// claims rows with a lease.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// auditOutbox is the platform-audit wiring main owns: one Enqueuer for the
// repositories and one Runner for delivery.
type auditOutbox struct {
	enq         *auditoutbox.Enqueuer
	runner      *auditoutbox.Runner
	maxMetadata int // AUDIT_MAX_METADATA_BYTES, for the repository's pre-enqueue validation
}

// auditEnvLookup reads the AUDIT_* environment, defaulting
// AUDIT_SOURCE_SERVICE to this service's name. legal.terms.accepted is
// producer-restricted to iam-realm-provisioner, so the value must not differ
// from domain.Source; the default keeps an unset variable from failing
// startup.
func auditEnvLookup(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	if name == auditconfig.EnvSourceService && v == "" {
		return domain.Source, true
	}
	return v, ok
}

// newAuditOutbox builds the Enqueuer and Runner. Every failure is fatal for
// the caller: a malformed AUDIT_* value, an app role that cannot INSERT into
// pending_audit_entries (Enqueue would otherwise swallow SQLSTATE 42501 and
// lose every entry), or a runner pool that cannot see every tenant's rows.
// An unset AUDIT_LOG_BASE_URL is not fatal here: delivery is disabled,
// entries queue durably and the backlog gauges report them. Outside
// APP_ENV=dev, validateRequiredEnv refuses to start without it first.
func newAuditOutbox(ctx context.Context, appPool, sysPool *pgcommon.Pool, log port.Logger) (*auditOutbox, error) {
	cfg, err := auditconfig.FromLookup("", auditEnvLookup)
	if err != nil {
		return nil, err
	}
	if cfg.SourceService != domain.Source {
		return nil, fmt.Errorf("%w: AUDIT_SOURCE_SERVICE must be %q (legal.terms.accepted is producer-restricted to it), got %q",
			audit.ErrConfig, domain.Source, cfg.SourceService)
	}
	labels := gincommon.MetricsConstLabels()
	warnings, err := audit.InitMetrics(audit.MetricsIdentity{
		Domain: labels["domain"], Service: labels["service"], Environment: labels["environment"],
	}, gincommon.MetricsRegisterer())
	if err != nil {
		return nil, fmt.Errorf("init platform-audit metrics: %w", err)
	}
	for _, w := range warnings {
		log.Warn("platform-audit metric registration warning", map[string]any{"metric": w.Metric, "error": w.Error()})
	}
	if err := auditoutbox.CheckEnqueueAccess(ctx, appPool); err != nil {
		return nil, err
	}
	enq, err := auditoutbox.NewEnqueuer(cfg.EnqueueConfig(log))
	if err != nil {
		return nil, err
	}
	client, err := cfg.NewClient(log)
	if err != nil {
		return nil, err
	}
	if client == nil {
		log.Warn("audit-entry delivery disabled — AUDIT_LOG_BASE_URL unset; TenantIdpConfigChanged and TermsAccepted entries accumulate in pending_audit_entries until it is set", nil)
	}
	runner, err := auditoutbox.NewRunner(cfg.RunnerConfig(sysPool, client, enq, log))
	if err != nil {
		return nil, err
	}
	return &auditOutbox{enq: enq, runner: runner, maxMetadata: cfg.MaxMetadataBytes}, nil
}

// start runs the Runner until ctx is canceled or stop is called.
func (a *auditOutbox) start(ctx context.Context, log port.Logger) {
	go func() {
		if err := a.runner.Start(ctx); err != nil {
			log.Error("platform-audit runner stopped with error", map[string]any{"error": err.Error()})
		}
	}()
}

// stop waits for the in-flight batch, bounded by timeout.
func (a *auditOutbox) stop(timeout time.Duration, log port.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := a.runner.Stop(ctx); err != nil {
		log.Error("platform-audit runner stop error", map[string]any{"error": err.Error()})
	}
}
