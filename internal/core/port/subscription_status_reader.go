package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TenantSubscriptionInfo is the subset of O&M's `tenants` row RP-C3 needs
// to drive the billing-lapse sweep (§8.9): subscription_status plus the
// timestamps/source that gate the cancelled->suspended->offboarded clock.
type TenantSubscriptionInfo struct {
	TenantID           uuid.UUID
	SubscriptionStatus string // active|trial|trial_expired|cancelled|suspended|offboarded (O&M's enum)
	CancelledAt        *time.Time
	SuspendedAt        *time.Time
	SuspensionSource   string // billing_lapse|operator|"" — RP-C3 excludes "operator" from auto-offboard (§8.8)
}

// SubscriptionStatusReader is RP-C3's single read dependency on Org &
// Membership: the daily lapse sweep needs every dedicated tenant's current
// subscription_status + timestamps (§8.9, "read from O&M").
//
// OPEN ITEM: the frozen LLD (§8.9/§16) specifies that this read happens
// but does not name the O&M endpoint that serves it — no such bulk/listing
// endpoint appears in O&M's own committed API surface as of this LLD's
// sign-off. Coordinate with the Org & Membership team before wiring the
// concrete adapter; this interface captures the shape RP-C3 needs so the
// reconciler's transition logic can be built and tested against a mock now
// without guessing at (and potentially locking in) an unratified contract.
type SubscriptionStatusReader interface {
	ListDedicatedTenantSubscriptions(ctx context.Context) ([]TenantSubscriptionInfo, error)
}
