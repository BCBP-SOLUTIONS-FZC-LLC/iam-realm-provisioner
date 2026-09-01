package domain

import (
	"time"

	"github.com/google/uuid"
)

// IdpConfig mirrors a `keycloak_idp_configs` row (§4.3). Secrets never live
// here — SAML signing keys and OIDC brokered-client secrets are in OpenBao
// (RP-D10); Config holds only non-secret protocol parameters.
type IdpConfig struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	IdpAlias      string
	Protocol      IdpProtocol
	DisplayName   string
	Enabled       bool
	Config        map[string]any
	Status        IdpConfigStatus
	RecordVersion int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IdpConfigValidation mirrors a `keycloak_idp_config_validations` row
// (§4.3) — one append-only row per §11.6 validation run.
type IdpConfigValidation struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	IdpAlias         string
	ValidatorVersion int
	Outcome          ValidationOutcome
	Checks           map[string]string // check_id -> pass|warn|fail
	Acknowledgements []IdpValidationAck
	ValidatedAt      time.Time
}

// IdpValidationAck records an admin's acknowledgement of a WARN-level §11.6
// check (RP-INV-4).
type IdpValidationAck struct {
	CheckID string    `json:"check_id"`
	UserID  uuid.UUID `json:"user_id"`
	At      time.Time `json:"at"`
}
