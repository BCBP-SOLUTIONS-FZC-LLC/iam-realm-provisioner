// Package eventschema embeds the JSON Schema documents used to validate
// outbound event payloads before they are written to the transactional
// outbox (internal/adapter/outbound/eventbus/validating_codec.go). One
// schema per published event type (§7.5, §23) — the 9 frozen event names.
// These are payload-only schemas (they describe the shape of the
// envelope's `data` field, not the envelope itself); see api/asyncapi.yaml
// for the full documented contract including the envelope and the 4
// consumed message types, which are not validated by this package — this
// service only produces the 9 schemas embedded here.
package eventschema

import _ "embed"

// TrialTenantProvisioned is the embedded JSON Schema for the TrialTenantProvisioned event payload.
//
//go:embed trial_tenant_provisioned.json
var TrialTenantProvisioned []byte

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

// ByEventType maps the frozen PascalCase event type name (§23) to its
// embedded schema, for the validating codec to compile at startup.
var ByEventType = map[string][]byte{
	"TrialTenantProvisioned": TrialTenantProvisioned,
	"TenantRealmReady":       TenantRealmReady,
	"TenantConverted":        TenantConverted,
	"DirectPaidSignup":       DirectPaidSignup,
	"TrialExpired":           TrialExpired,
	"TrialReactivated":       TrialReactivated,
	"TenantSuspended":        TenantSuspended,
	"TenantOffboarded":       TenantOffboarded,
	"TenantReactivated":      TenantReactivated,
}
