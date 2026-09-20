package domain

import "fmt"

// ErrorCode is one of the frozen §17 error taxonomy codes, surfaced through
// platform-gincommon's flat ErrorResponse shape
// ({error, status, trace_id, request_id, details?}).
type ErrorCode string

// ErrorCode values (§17).
const (
	ErrInvalidRealmStrategy           ErrorCode = "invalid_realm_strategy"             // 400
	ErrInvalidIdpProtocol             ErrorCode = "invalid_idp_protocol"               // 400
	ErrUnsupportedRealmSetting        ErrorCode = "unsupported_realm_setting"          // 400
	ErrIdpRegionPortabilityFailed     ErrorCode = "idp_region_portability_failed"      // 400
	ErrMissingIdempotencyKey          ErrorCode = "missing_idempotency_key"            // 400
	ErrIdpWarningsUnacknowledged      ErrorCode = "idp_warnings_unacknowledged"        // 422
	ErrTenantRealmNotFound            ErrorCode = "tenant_realm_not_found"             // 404
	ErrIdpConfigNotFound              ErrorCode = "idp_config_not_found"               // 404
	ErrKeycloakUserNotFound           ErrorCode = "keycloak_user_not_found"            // 404
	ErrIdpRequiresDedicatedRealm      ErrorCode = "idp_requires_dedicated_realm"       // 409
	ErrRealmAlreadyDedicated          ErrorCode = "realm_already_dedicated"            // 409
	ErrRevertWindowExpired            ErrorCode = "revert_window_expired"              // 409
	ErrTenantOffboarded               ErrorCode = "tenant_offboarded"                  // 409
	ErrOperationInProgress            ErrorCode = "operation_in_progress"              // 409
	ErrOptimisticLockConflict         ErrorCode = "optimistic_lock_conflict"           // 409
	ErrUserEmailConflict              ErrorCode = "user_email_conflict"                // 409
	ErrServiceAccountRotationInFlight ErrorCode = "service_account_rotation_in_flight" // 409
	ErrServiceAccountNotProvisioned   ErrorCode = "service_account_not_provisioned"    // 422
	ErrKeycloakUnavailable            ErrorCode = "keycloak_unavailable"               // 502
	ErrConversionFailed               ErrorCode = "conversion_failed"                  // 502
	ErrOrgMembershipUnavailable       ErrorCode = "org_membership_unavailable"         // 503
	ErrObjectStoreUnavailable         ErrorCode = "object_store_unavailable"           // 503
	ErrDBUnavailable                  ErrorCode = "db_unavailable"                     // 503
	ErrTokenServiceUnavailable        ErrorCode = "token_service_unavailable"          // 503

	// ErrInvalidRequest is additive to the frozen §17 taxonomy -- a generic
	// malformed-JSON/failed-binding-validation 400, distinct from the
	// named business-validation codes above.
	ErrInvalidRequest ErrorCode = "invalid_request" // 400
)

// ErrKeycloakUserNotFound and ErrUserEmailConflict are additive to the
// frozen §17 taxonomy (same pattern as ErrInvalidRequest above): resolving
// a create-user 409 into "lost-response retry of my own request" vs "a
// genuine conflict" (§9.2) needs both a distinguishable not-found signal
// from FindUserByEmail and a distinguishable conflict signal from
// CreateTrialUser/CreateInvitedUser — previously both folded into the
// generic ErrKeycloakUnavailable (502), which a caller could never tell
// apart from an actual Keycloak outage.

// httpStatus maps every ErrorCode to its frozen HTTP status (§17).
var httpStatus = map[ErrorCode]int{
	ErrInvalidRequest:                 400,
	ErrInvalidRealmStrategy:           400,
	ErrInvalidIdpProtocol:             400,
	ErrUnsupportedRealmSetting:        400,
	ErrIdpRegionPortabilityFailed:     400,
	ErrMissingIdempotencyKey:          400,
	ErrIdpWarningsUnacknowledged:      422,
	ErrServiceAccountNotProvisioned:   422,
	ErrTenantRealmNotFound:            404,
	ErrIdpConfigNotFound:              404,
	ErrKeycloakUserNotFound:           404,
	ErrIdpRequiresDedicatedRealm:      409,
	ErrRealmAlreadyDedicated:          409,
	ErrRevertWindowExpired:            409,
	ErrTenantOffboarded:               409,
	ErrOperationInProgress:            409,
	ErrOptimisticLockConflict:         409,
	ErrUserEmailConflict:              409,
	ErrServiceAccountRotationInFlight: 409,
	ErrKeycloakUnavailable:            502,
	ErrConversionFailed:               502,
	ErrOrgMembershipUnavailable:       503,
	ErrObjectStoreUnavailable:         503,
	ErrDBUnavailable:                  503,
	ErrTokenServiceUnavailable:        503,
}

// Error is this service's domain error type: a stable code, an HTTP status
// derived from that code, a human message, and optional structured details
// echoed verbatim into ErrorResponse.details.
type Error struct {
	Code    ErrorCode
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Status returns the frozen HTTP status for e's code.
func (e *Error) Status() int {
	if s, ok := httpStatus[e.Code]; ok {
		return s
	}
	return 500
}

// NewError constructs a domain Error for code with message.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WithDetails attaches structured details and returns e for chaining.
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}
