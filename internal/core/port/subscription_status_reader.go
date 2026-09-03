package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// LapsedSubscription is one row of O&M's I-16 response
// (GET /api/v1/internal/subscription-lapses, §16 RP-C3 of the RP↔O&M
// alignment review): a tenant whose subscription is `status='cancelled'`
// and past O&M's own SUBSCRIPTION_GRACE_DAYS. O&M does the grace-period
// math so the 30-day rule stays single-sourced; RP only ever sees tenants
// already due for the suspend action — the poll is self-idempotent,
// since RP suspending a tenant (emitting TenantSuspended) naturally drops
// it from O&M's next response.
type LapsedSubscription struct {
	TenantID    uuid.UUID
	RealmID     string
	RealmType   string // "shared"|"dedicated" — RP-C3 only acts on "dedicated"; a "shared" entry is a trial tenant, owned by RP-C1/RP-C2 instead
	CancelledAt time.Time
}

// SubscriptionLapseReader is RP-C3's single read dependency on Org &
// Membership for the SUSPEND half of the billing-lapse sweep (§8.8/§8.9):
// which dedicated tenants are cancelled and past grace. The OFFBOARD half
// (a billing-lapse-suspended tenant past RETENTION_DAYS) is answered
// entirely from this service's own tenant_realms.suspension_source —
// O&M's endpoint carries no notion of "already suspended," so it is never
// consulted for that decision (port.TenantRealmRepository.
// ListSuspendedPastRetention).
type SubscriptionLapseReader interface {
	ListLapsedSubscriptions(ctx context.Context) ([]LapsedSubscription, error)
}
