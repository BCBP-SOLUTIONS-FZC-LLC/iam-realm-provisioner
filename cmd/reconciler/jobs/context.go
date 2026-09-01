// Package jobs holds the concrete reconciler bodies for the RP-C1..RP-C4
// CronJobs (§8.7/§8.9/§8.10) plus the outbox/processed_events prune jobs.
// Every job is package-visible so cmd/reconciler's dispatcher can wire it
// into the --job registry without touching implementation details.
package jobs

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// Context is the dependency bag every reconciler function accepts.
// Populated by cmd/reconciler/main.go from env + Postgres/Keycloak/S3
// clients.
type Context struct {
	Pool         *pgcommon.Pool
	SysPool      *pgcommon.Pool // BYPASSRLS pool (§10.1) — no GUCProvider, sees across every tenant
	OutboxRunner *outbox.Runner
	TxRunner     port.TxRunner

	TenantRealms       port.TenantRealmRepository
	Reconciliations    port.ReconciliationRepository
	Idempotency        port.IdempotencyStore
	ProcessedEvents    port.ProcessedEventsStore
	Keycloak           port.KeycloakAdmin
	ObjectStore        port.ObjectStore
	SubscriptionStatus port.SubscriptionStatusReader // optional — see its doc comment (open O&M coordination item)

	Logger port.SlogStyleLogger

	BatchLimit             int
	TrialGraceDays         int
	CancelledGraceDays     int
	RetentionDays          int
	OutboxRetentionDays    int
	ProcessedEventsTTLDays int
}

// Result summarizes a reconciler run.
type Result struct {
	Attempted int
	Succeeded int
	Failed    int
	Skipped   int
}

// Func is the signature every job satisfies.
type Func func(ctx context.Context, jctx *Context) (Result, error)

// failIdempotency marks idemKey's provisioning_operations row failed,
// logging (not propagating) a failure to do so — the caller already has
// its own error/skip reason to record via Result, so this is best-effort.
func failIdempotency(ctx context.Context, jctx *Context, idemKey string) {
	if err := jctx.Idempotency.Fail(ctx, idemKey); err != nil {
		jctx.Logger.WarnContext(ctx, "idempotency: mark failed did not persist", "idempotency_key", idemKey, "error", err.Error())
	}
}

// marshalIdempotencyResult marshals v for storage in the idempotency
// ledger. A marshal failure is logged, not propagated — the caller stores
// whatever bytes result, matching the pre-existing best-effort semantics.
func marshalIdempotencyResult(ctx context.Context, jctx *Context, op string, v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		jctx.Logger.WarnContext(ctx, op+": marshal idempotency result failed", "error", err.Error())
	}
	return body
}
