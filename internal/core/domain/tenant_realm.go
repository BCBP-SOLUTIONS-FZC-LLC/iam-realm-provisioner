package domain

import (
	"time"

	"github.com/google/uuid"
)

// TenantRealm mirrors the `tenant_realms` row (§4.2) — this service's
// authoritative record of what it created in Keycloak for a tenant. It is
// NOT a copy of Org & Membership's `tenants` row (RP-D6).
type TenantRealm struct {
	TenantID             uuid.UUID
	RealmName            string
	Strategy             RealmStrategy
	KeycloakShard        *string
	Status               RealmStatus
	RealmTemplateVersion *string
	TrialEndsAt          *time.Time
	ConvertedAt          *time.Time
	DisabledAt           *time.Time
	SuspensionSource     *SuspensionSource
	ExportedAt           *time.Time
	ExportS3Key          *string
	RecordVersion        int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time
}

// IsOffboarded reports whether the realm has reached the terminal state
// (RP-INV-5) — no transition out is permitted.
func (t *TenantRealm) IsOffboarded() bool {
	return t.Status == RealmStatusDeleted || t.Status == RealmStatusExported
}

// IsDedicated reports whether the tenant has a dedicated realm (post
// conversion or direct-paid signup).
func (t *TenantRealm) IsDedicated() bool {
	return t.Strategy == RealmStrategyDedicated
}
