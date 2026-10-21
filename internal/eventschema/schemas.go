// Package eventschema embeds the JSON Schema documents used to validate
// outbound event payloads before they are written to the transactional
// outbox (internal/adapter/outbound/eventbus/validating_codec.go). One
// schema per published event type (§7.5, §25) — the 9 frozen event names.
// These are payload-only schemas (they describe the shape of the
// envelope's `data` field, not the envelope itself); see api/asyncapi.yaml
// for the full documented contract including the envelope.
//
// The directory also holds the 6 consumed-message schemas schema-gov
// extracts from api/asyncapi.yaml (department_membership_*, tenant_role_*,
// trial_started, billing_tenant_reactivated). They are NOT in ByEventType
// — never registered in Glue (stage-produced-event-schemas.sh skips them)
// nor used to validate outbound events — and are exposed separately as
// ConsumedByConsumer, which the inbound SQS consumers validate against.
package eventschema

import _ "embed"

// TenantIdentityProvisioned is the embedded JSON Schema for the TenantIdentityProvisioned event payload.
//
//go:embed tenant_identity_provisioned.json
var TenantIdentityProvisioned []byte

// TenantRealmReady is the embedded JSON Schema for the TenantRealmReady event payload.
//
//go:embed tenant_realm_ready.json
var TenantRealmReady []byte

// TenantConverted is the embedded JSON Schema for the TenantConverted event payload.
//
//go:embed tenant_converted.json
var TenantConverted []byte

// DirectPaidSignup is the embedded JSON Schema for the DirectPaidSignup event payload.
//
//go:embed direct_paid_signup.json
var DirectPaidSignup []byte

// TrialExpired is the embedded JSON Schema for the TrialExpired event payload.
//
//go:embed trial_expired.json
var TrialExpired []byte

// TrialReactivated is the embedded JSON Schema for the TrialReactivated event payload.
//
//go:embed trial_reactivated.json
var TrialReactivated []byte

// TenantSuspended is the embedded JSON Schema for the TenantSuspended event payload.
//
//go:embed tenant_suspended.json
var TenantSuspended []byte

// TenantOffboarded is the embedded JSON Schema for the TenantOffboarded event payload.
//
//go:embed tenant_offboarded.json
var TenantOffboarded []byte

// TenantReactivated is the embedded JSON Schema for the TenantReactivated event payload.
//
//go:embed tenant_reactivated.json
var TenantReactivated []byte

// ByEventType maps the frozen PascalCase event type name (§25) to its
// embedded schema, for the validating codec to compile at startup.
var ByEventType = map[string][]byte{
	"TenantIdentityProvisioned": TenantIdentityProvisioned,
	"TenantRealmReady":          TenantRealmReady,
	"TenantConverted":           TenantConverted,
	"DirectPaidSignup":          DirectPaidSignup,
	"TrialExpired":              TrialExpired,
	"TrialReactivated":          TrialReactivated,
	"TenantSuspended":           TenantSuspended,
	"TenantOffboarded":          TenantOffboarded,
	"TenantReactivated":         TenantReactivated,
}

// DepartmentMembershipGranted is the consumed (O&M-owned) DepartmentMembershipGranted payload schema.
//
//go:embed department_membership_granted.json
var DepartmentMembershipGranted []byte

// DepartmentMembershipLevelChanged is the consumed (O&M-owned) DepartmentMembershipLevelChanged payload schema.
//
//go:embed department_membership_level_changed.json
var DepartmentMembershipLevelChanged []byte

// TenantRoleGranted is the consumed (O&M-owned) TenantRoleGranted payload schema.
//
//go:embed tenant_role_granted.json
var TenantRoleGranted []byte

// TenantRoleRevoked is the consumed (O&M-owned) TenantRoleRevoked payload schema.
//
//go:embed tenant_role_revoked.json
var TenantRoleRevoked []byte

// TrialStarted is the consumed (O&M-owned) TrialStarted payload schema.
//
//go:embed trial_started.json
var TrialStarted []byte

// BillingTenantReactivated is the consumed (Billing-owned) TenantReactivated
// payload schema — a different message from this service's own produced
// TenantReactivated (RP-D13), riding a different topic.
//
//go:embed billing_tenant_reactivated.json
var BillingTenantReactivated []byte

// ConsumedByConsumer maps each inbound consumer (the frozen §25
// processed_events.consumer value) to the payload schema for every envelope
// type it handles. Keyed per consumer, not globally, because envelope types
// are not unique across producers: billing-realm-q's TenantReactivated is
// Billing's, and must never be validated against this service's own
// produced TenantReactivated in ByEventType (source=operator).
var ConsumedByConsumer = map[string]map[string][]byte{
	"membership": {
		"DepartmentMembershipGranted":      DepartmentMembershipGranted,
		"DepartmentMembershipLevelChanged": DepartmentMembershipLevelChanged,
		"TenantRoleGranted":                TenantRoleGranted,
		"TenantRoleRevoked":                TenantRoleRevoked,
	},
	"tenant": {
		"TrialStarted": TrialStarted,
	},
	"billing": {
		"TenantReactivated": BillingTenantReactivated,
	},
}
