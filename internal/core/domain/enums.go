// Package domain holds the Realm Provisioner's entities, value objects, and
// invariants. It imports no adapter, Gin, pgx, gocloak, or AWS SDK package
// (enforced by .go-arch-lint.yml) — every name here is frozen by LLD §23 and
// must match character-for-character.
package domain

// RealmStrategy is the Postgres enum `realm_strategy` (§4.1, §23).
type RealmStrategy string

// RealmStrategy values (§23).
const (
	RealmStrategyShared    RealmStrategy = "shared"
	RealmStrategyDedicated RealmStrategy = "dedicated"
)

// RealmStatus is the Postgres enum `realm_status` (§4.1, §23).
type RealmStatus string

// RealmStatus values (§23).
const (
	RealmStatusProvisioning RealmStatus = "provisioning"
	RealmStatusActive       RealmStatus = "active"
	RealmStatusDisabled     RealmStatus = "disabled"
	RealmStatusSuspended    RealmStatus = "suspended"
	RealmStatusExported     RealmStatus = "exported"
	RealmStatusDeleted      RealmStatus = "deleted"
)

// IdpProtocol is the Postgres enum `idp_protocol` (§4.1, §23).
type IdpProtocol string

// IdpProtocol values (§23).
const (
	IdpProtocolSAML IdpProtocol = "saml"
	IdpProtocolOIDC IdpProtocol = "oidc"
)

// IdpConfigStatus is the Postgres enum `idp_config_status` (§4.1, §23).
type IdpConfigStatus string

// IdpConfigStatus values (§23).
const (
	IdpConfigStatusActive            IdpConfigStatus = "active"
	IdpConfigStatusDisabled          IdpConfigStatus = "disabled"
	IdpConfigStatusPendingValidation IdpConfigStatus = "pending_validation"
)

// ValidationOutcome is the Postgres enum `validation_outcome` (§4.1, §23).
type ValidationOutcome string

// ValidationOutcome values (§23).
const (
	ValidationOutcomePass         ValidationOutcome = "pass"
	ValidationOutcomeWarningsOnly ValidationOutcome = "warnings_only"
	ValidationOutcomeFailing      ValidationOutcome = "failing"
)

// ReconStatus is the Postgres enum `recon_status` (§4.1, §23).
type ReconStatus string

// ReconStatus values (§23).
const (
	ReconStatusPending ReconStatus = "pending"
	ReconStatusApplied ReconStatus = "applied"
	ReconStatusFailed  ReconStatus = "failed"
)

// OpStatus is the Postgres enum `op_status` (§4.1, §23).
type OpStatus string

// OpStatus values (§23).
const (
	OpStatusInProgress OpStatus = "in_progress"
	OpStatusSucceeded  OpStatus = "succeeded"
	OpStatusFailed     OpStatus = "failed"
)

// ProcessedEventsConsumer is the frozen `processed_events.consumer`
// discriminator (§23) — one value per inbound SQS queue.
type ProcessedEventsConsumer string

// ProcessedEventsConsumer values (§23), one per inbound SQS queue.
const (
	ConsumerMembership ProcessedEventsConsumer = "membership"
	ConsumerTenant     ProcessedEventsConsumer = "tenant"
	ConsumerBilling    ProcessedEventsConsumer = "billing"
)

// SuspensionSource is the frozen `TenantSuspended.source` value set (§23).
type SuspensionSource string

// SuspensionSource values (§23).
const (
	SuspensionSourceBillingLapse SuspensionSource = "billing_lapse"
	SuspensionSourceOperator     SuspensionSource = "operator"
)

// ReactivationSource is the frozen `TenantReactivated.source` value set (§23).
type ReactivationSource string

// ReactivationSource values (§23).
const (
	ReactivationSourceBilling  ReactivationSource = "billing"
	ReactivationSourceOperator ReactivationSource = "operator"
)
