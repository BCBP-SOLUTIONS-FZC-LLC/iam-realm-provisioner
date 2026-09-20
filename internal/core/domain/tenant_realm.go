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
	// ServiceAccountPrincipalID/ServiceAccountKeycloakClientID record the
	// tenant's platform-automation principal (§2.5, RP-17) once minted at
	// RP-1/RP-2/RP-3 — nil until then. KeycloakClientID is the literal
	// base name "platform-automation" for a dedicated realm, or that name
	// suffixed with the tenant's UUID for the shared trial realm (a shared
	// realm cannot hold two clients with the same clientId).
	ServiceAccountPrincipalID      *uuid.UUID
	ServiceAccountKeycloakClientID *string
	RecordVersion                  int64
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	DeletedAt                      *time.Time
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
