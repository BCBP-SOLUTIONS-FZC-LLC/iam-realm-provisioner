package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ProvisioningOperation mirrors a `provisioning_operations` row (§4.4) — the
// idempotency ledger. Every externally-triggered mutation (RP-1..RP-10,
// RP-12, RP-14, and the three SQS handlers) is keyed here (§9.2).
type ProvisioningOperation struct {
	IdempotencyKey string
	TenantID       uuid.UUID
	Operation      string
	Status         OpStatus
	Result         json.RawMessage
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

// Operation names — additive per-endpoint discriminators for
// `provisioning_operations.operation` (§23 notes this set is additive, not
// enumerated exhaustively in the frozen inventory).
const (
	OpProvisionTrial    = "provision_trial"
	OpProvisionDirect   = "provision_direct"
	OpConvert           = "convert"
	OpRevertConversion  = "revert_conversion"
	OpCreateInvitedUser = "create_invited_user"
	OpConfigureIdp      = "configure_idp"
	OpRemoveIdp         = "remove_idp"
	OpApplyRealmConfig  = "apply_realm_config"
	OpMFAReset          = "mfa_reset"
	OpReactivate        = "reactivate"
	OpExpireTrial       = "expire_trial"
	OpSuspend           = "suspend"
	OpDeleteUser        = "delete_user"
	OpRevokeSessions    = "revoke_sessions"
)

// RealmConfigReconciliation mirrors a `realm_config_reconciliations` row
// (§4.4) — backs RP-INV-6 durability for settings whose Keycloak mutation
// has not yet landed.
type RealmConfigReconciliation struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	SettingKey   string
	DesiredValue json.RawMessage
	Status       ReconStatus
	Attempts     int
	LastError    *string
	CreatedAt    time.Time
	AppliedAt    *time.Time
}

// AllowedRealmSettingKeys is the allow-list realm-config `setting_key` must
// belong to (§10.4) — else 400 unsupported_realm_setting. `idp:<alias>`
// keys (federation toggles) are additionally allowed via IsAllowedSettingKey.
var AllowedRealmSettingKeys = map[string]bool{
	"local_accounts_enabled": true,
}

// IsAllowedSettingKey reports whether key is on the realm-config allow-list
// — either an exact match in AllowedRealmSettingKeys or an `idp:<alias>`
// federation-toggle key (§4.4).
func IsAllowedSettingKey(key string) bool {
	if AllowedRealmSettingKeys[key] {
		return true
	}
	return len(key) > 4 && key[:4] == "idp:"
}
