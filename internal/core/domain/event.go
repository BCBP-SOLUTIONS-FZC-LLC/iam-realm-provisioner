package domain

import (
	"encoding/json"

	"github.com/google/uuid"
)

// Source is the frozen `source` value stamped on every envelope this
// service publishes (§25).
const Source = "iam-realm-provisioner"

// SystemActorID is the sentinel actor used on cron-origin events (§7.4) —
// TrialExpired, TenantSuspended (lapse sweep), TenantOffboarded.
const SystemActorID = "00000000-0000-0000-0000-0000000000a1"

// The 9 frozen published event type names (§25, §7.5) — the values of the
// envelope `type` field, and the Glue schema names registered in the shared
// `iam-tenant-events` registry.
const (
	EventTrialTenantProvisioned = "TrialTenantProvisioned"
	EventTenantRealmReady       = "TenantRealmReady"
	EventTenantConverted        = "TenantConverted"
	EventDirectPaidSignup       = "DirectPaidSignup"
	EventTrialExpired           = "TrialExpired"
	EventTrialReactivated       = "TrialReactivated"
	EventTenantSuspended        = "TenantSuspended"
	EventTenantOffboarded       = "TenantOffboarded"
	EventTenantReactivated      = "TenantReactivated"
)

// The 4 consumed event type names (§7.1) this service subscribes to across
// its three inbound SQS queues.
const (
	EventDepartmentMembershipGranted      = "DepartmentMembershipGranted"
	EventDepartmentMembershipLevelChanged = "DepartmentMembershipLevelChanged"
	EventTenantRoleGranted                = "TenantRoleGranted"
	EventTenantRoleRevoked                = "TenantRoleRevoked"
	EventTrialStarted                     = "TrialStarted"
	// EventTenantReactivatedBilling is the SAME type name as
	// EventTenantReactivated but arrives on a different topic
	// (billing.events -> billing-realm-q, source=billing) — RP-D13, no
	// self-consumption since produced/consumed ride different topics.
)

// Event is the input to port.EventPublisher.Enqueue — everything
// needed to build a platform-events envelope and validate its payload
// before it is written to the outbox inside the caller's transaction
// (RP-INV-3).
type Event struct {
	Type      string
	TenantID  uuid.UUID
	Subject   string
	Actor     string
	IPAddress string
	UserAgent string
	Data      any
}

// TrialTenantProvisionedPayload is the frozen payload for
// TrialTenantProvisioned (§25, §7.5) — emitted by RP-1.
type TrialTenantProvisionedPayload struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	Realm          string    `json:"realm"`
	KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
}

// TenantRealmReadyPayload is the frozen payload for TenantRealmReady (§25,
// §7.5) — emitted by RP-2 or RP-3 (never for trial).
type TenantRealmReadyPayload struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	Realm         string    `json:"realm"`
	KeycloakShard string    `json:"keycloak_shard"`
	OIDCClients   []string  `json:"oidc_clients"`
}

// TenantConvertedPayload is the frozen payload for TenantConverted (§25,
// §7.5) — emitted by RP-3 (or RP-2 with direct_signup=true).
type TenantConvertedPayload struct {
	TenantID              uuid.UUID `json:"tenant_id"`
	Realm                 string    `json:"realm"`
	Plan                  string    `json:"plan"`
	BillingSubscriptionID *string   `json:"billing_subscription_id,omitempty"`
	UsersMigrated         int       `json:"users_migrated"`
	DirectSignup          bool      `json:"direct_signup"`
}

// DirectPaidSignupPayload is the frozen payload for DirectPaidSignup (§25,
// §7.5) — emitted by RP-2 completion.
type DirectPaidSignupPayload struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	Realm        string    `json:"realm"`
	Plan         string    `json:"plan"`
	DirectSignup bool      `json:"direct_signup"`
}

// TrialExpiredPayload is the frozen payload for TrialExpired (§25, §7.5) —
// emitted by RP-C1/RP-12.
type TrialExpiredPayload struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	TrialEndsAt string    `json:"trial_ends_at"`
	GraceEndsAt string    `json:"grace_ends_at"`
}

// TrialReactivatedPayload is the frozen payload for TrialReactivated (§25,
// §7.5) — emitted by RP-10 (trial branch).
type TrialReactivatedPayload struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	NewTrialEndsAt string    `json:"new_trial_ends_at"`
}

// TenantSuspendedPayload is the frozen payload for TenantSuspended (§25,
// §7.5) — emitted by RP-C3 lapse sweep (source=billing_lapse) or RP-14
// (source=operator).
type TenantSuspendedPayload struct {
	TenantID    uuid.UUID        `json:"tenant_id"`
	Realm       string           `json:"realm"`
	CancelledAt *string          `json:"cancelled_at,omitempty"`
	SuspendedAt string           `json:"suspended_at"`
	Source      SuspensionSource `json:"source"`
	Reason      string           `json:"reason,omitempty"`
}

// TenantOffboardedPayload is the frozen payload for TenantOffboarded (§25,
// §7.5) — emitted by RP-C3 after confirmed S3 export + realm delete.
type TenantOffboardedPayload struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	Realm        string    `json:"realm"`
	ExportS3Key  string    `json:"export_s3_key"`
	OffboardedAt string    `json:"offboarded_at"`
}

// TenantReactivatedPayload is the frozen payload for TenantReactivated
// (§25, §7.5) — produced here (RP-10 paid branch, source=operator) and
// consumed from Billing (source=billing, different topic — RP-D13).
type TenantReactivatedPayload struct {
	TenantID uuid.UUID          `json:"tenant_id"`
	Realm    string             `json:"realm"`
	Source   ReactivationSource `json:"source"`
	Reason   string             `json:"reason,omitempty"`
}

// Consumed payload shapes (§7.1, cross-checked against
// iam-org-membership's api/asyncapi.yaml — the producer's own contract).

// DepartmentMembershipGrantedPayload mirrors O&M's
// DepartmentMembershipGrantedPayload.
type DepartmentMembershipGrantedPayload struct {
	UserID       uuid.UUID `json:"user_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	DepartmentID uuid.UUID `json:"department_id"`
	Level        string    `json:"level"` // preparator|reviewer|approver
	ActorID      uuid.UUID `json:"actor_id"`
}

// DepartmentMembershipLevelChangedPayload mirrors O&M's
// DepartmentMembershipLevelChangedPayload.
type DepartmentMembershipLevelChangedPayload struct {
	UserID        uuid.UUID `json:"user_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	DepartmentID  uuid.UUID `json:"department_id"`
	PreviousLevel string    `json:"previous_level"`
	NewLevel      string    `json:"new_level"`
	ActorID       uuid.UUID `json:"actor_id"`
}

// TenantRoleGrantedPayload mirrors O&M's TenantRoleGrantedPayload.
type TenantRoleGrantedPayload struct {
	UserID   uuid.UUID `json:"user_id"`
	TenantID uuid.UUID `json:"tenant_id"`
	RoleCode string    `json:"role_code"` // tenant_owner|tenant_admin|tender_admin
	ActorID  uuid.UUID `json:"actor_id"`
}

// TenantRoleRevokedPayload mirrors O&M's TenantRoleRevokedPayload.
type TenantRoleRevokedPayload struct {
	UserID   uuid.UUID `json:"user_id"`
	TenantID uuid.UUID `json:"tenant_id"`
	RoleCode string    `json:"role_code"`
	ActorID  uuid.UUID `json:"actor_id"`
}

// TrialStartedPayload mirrors O&M's TrialStartedPayload.
type TrialStartedPayload struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	Plan        string    `json:"plan"`
	TrialEndsAt string    `json:"trial_ends_at"`
}

// BillingTenantReactivatedPayload mirrors Billing's TenantReactivated
// payload on the `billing.events` topic (source=billing) — distinct from
// this service's own produced TenantReactivatedPayload (RP-D13).
type BillingTenantReactivatedPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Source   string    `json:"source"` // "billing"
	Reason   string    `json:"reason,omitempty"`
}

var _ = json.RawMessage(nil)
