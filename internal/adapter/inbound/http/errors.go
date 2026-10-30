package http

import (
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// ProblemResponse is the RFC 9457 problem details wire shape. It is referenced
// by Swagger @Failure annotations as the standard error response type.
type ProblemResponse = problem.Problem

// Domain problem types — one per §17 ErrorCode, mapping each frozen taxonomy
// code to an RFC 9457 type with domain="realm" and an UPPER_SNAKE_CASE code.
var (
	// 400 Bad Request
	realmInvalidRequest             = problem.MustType("realm", "invalid-request", "Bad Request", 400, "INVALID_REQUEST")
	realmInvalidRealmStrategy       = problem.MustType("realm", "invalid-realm-strategy", "Invalid Realm Strategy", 400, "INVALID_REALM_STRATEGY")
	realmInvalidIdpProtocol         = problem.MustType("realm", "invalid-idp-protocol", "Invalid IdP Protocol", 400, "INVALID_IDP_PROTOCOL")
	realmUnsupportedRealmSetting    = problem.MustType("realm", "unsupported-realm-setting", "Unsupported Realm Setting", 400, "UNSUPPORTED_REALM_SETTING")
	realmIdpRegionPortabilityFailed = problem.MustType("realm", "idp-region-portability-failed", "IdP Region Portability Failed", 400, "IDP_REGION_PORTABILITY_FAILED")
	realmMissingIdempotencyKey      = problem.MustType("realm", "missing-idempotency-key", "Missing Idempotency Key", 400, "MISSING_IDEMPOTENCY_KEY")
	// 401 Unauthorized
	realmMissingIdentityHeaders = problem.MustType("realm", "missing-identity-headers", "Missing Identity Headers", 401, "MISSING_IDENTITY_HEADERS")
	// 403 Forbidden
	realmInsufficientRole = problem.MustType("realm", "insufficient-role", "Insufficient Role", 403, "INSUFFICIENT_ROLE")
	realmTenantMismatch   = problem.MustType("realm", "tenant-mismatch", "Tenant Mismatch", 403, "TENANT_MISMATCH")
	// 404 Not Found
	realmTenantRealmNotFound  = problem.MustType("realm", "tenant-realm-not-found", "Tenant Realm Not Found", 404, "TENANT_REALM_NOT_FOUND")
	realmIdpConfigNotFound    = problem.MustType("realm", "idp-config-not-found", "IdP Config Not Found", 404, "IDP_CONFIG_NOT_FOUND")
	realmKeycloakUserNotFound = problem.MustType("realm", "keycloak-user-not-found", "Keycloak User Not Found", 404, "KEYCLOAK_USER_NOT_FOUND")
	// 409 Conflict
	realmIdpRequiresDedicatedRealm      = problem.MustType("realm", "idp-requires-dedicated-realm", "IdP Requires Dedicated Realm", 409, "IDP_REQUIRES_DEDICATED_REALM")
	realmAlreadyDedicated               = problem.MustType("realm", "realm-already-dedicated", "Realm Already Dedicated", 409, "REALM_ALREADY_DEDICATED")
	realmRevertWindowExpired            = problem.MustType("realm", "revert-window-expired", "Revert Window Expired", 409, "REVERT_WINDOW_EXPIRED")
	realmNameUnavailable                = problem.MustType("realm", "realm-name-unavailable", "Realm Name Unavailable", 409, "REALM_NAME_UNAVAILABLE")
	realmTenantOffboarded               = problem.MustType("realm", "tenant-offboarded", "Tenant Offboarded", 409, "TENANT_OFFBOARDED")
	realmOperationInProgress            = problem.MustType("realm", "operation-in-progress", "Operation In Progress", 409, "OPERATION_IN_PROGRESS")
	realmOptimisticLockConflict         = problem.MustType("realm", "optimistic-lock-conflict", "Optimistic Lock Conflict", 409, "OPTIMISTIC_LOCK_CONFLICT")
	realmUserEmailConflict              = problem.MustType("realm", "user-email-conflict", "User Email Conflict", 409, "USER_EMAIL_CONFLICT")
	realmServiceAccountRotationInFlight = problem.MustType("realm", "service-account-rotation-in-flight", "Service Account Rotation In Flight", 409, "SERVICE_ACCOUNT_ROTATION_IN_FLIGHT")
	realmCredentialReplayRevoked        = problem.MustType("realm", "credential-replay-revoked", "Credential Replay Revoked", 409, "CREDENTIAL_REPLAY_REVOKED")
	realmCredentialReplayExpired        = problem.MustType("realm", "credential-replay-expired", "Credential Replay Expired", 409, "CREDENTIAL_REPLAY_EXPIRED")
	// 422 Unprocessable Entity
	realmIdpWarningsUnacknowledged    = problem.MustType("realm", "idp-warnings-unacknowledged", "IdP Warnings Unacknowledged", 422, "IDP_WARNINGS_UNACKNOWLEDGED")
	realmServiceAccountNotProvisioned = problem.MustType("realm", "service-account-not-provisioned", "Service Account Not Provisioned", 422, "SERVICE_ACCOUNT_NOT_PROVISIONED")
	// 502 Bad Gateway
	realmKeycloakUnavailable = problem.MustType("realm", "keycloak-unavailable", "Keycloak Unavailable", 502, "KEYCLOAK_UNAVAILABLE")
	realmConversionFailed    = problem.MustType("realm", "conversion-failed", "Conversion Failed", 502, "CONVERSION_FAILED")
	// 503 Service Unavailable
	realmOrgMembershipUnavailable = problem.MustType("realm", "org-membership-unavailable", "Org Membership Unavailable", 503, "ORG_MEMBERSHIP_UNAVAILABLE")
	realmObjectStoreUnavailable   = problem.MustType("realm", "object-store-unavailable", "Object Store Unavailable", 503, "OBJECT_STORE_UNAVAILABLE")
	realmDBUnavailable            = problem.MustType("realm", "db-unavailable", "DB Unavailable", 503, "DB_UNAVAILABLE")
	realmTokenServiceUnavailable  = problem.MustType("realm", "token-service-unavailable", "Token Service Unavailable", 503, "TOKEN_SERVICE_UNAVAILABLE")
	realmRequestTimeout           = problem.MustType("realm", "request-timeout", "Request Timeout", 503, "REQUEST_TIMEOUT")
)

// domainCodeTypes maps each frozen §17 ErrorCode to its problem.Type.
var domainCodeTypes = map[domain.ErrorCode]problem.Type{
	domain.ErrInvalidRequest:                 realmInvalidRequest,
	domain.ErrInvalidRealmStrategy:           realmInvalidRealmStrategy,
	domain.ErrInvalidIdpProtocol:             realmInvalidIdpProtocol,
	domain.ErrUnsupportedRealmSetting:        realmUnsupportedRealmSetting,
	domain.ErrIdpRegionPortabilityFailed:     realmIdpRegionPortabilityFailed,
	domain.ErrMissingIdempotencyKey:          realmMissingIdempotencyKey,
	domain.ErrIdpWarningsUnacknowledged:      realmIdpWarningsUnacknowledged,
	domain.ErrServiceAccountNotProvisioned:   realmServiceAccountNotProvisioned,
	domain.ErrTenantRealmNotFound:            realmTenantRealmNotFound,
	domain.ErrIdpConfigNotFound:              realmIdpConfigNotFound,
	domain.ErrKeycloakUserNotFound:           realmKeycloakUserNotFound,
	domain.ErrIdpRequiresDedicatedRealm:      realmIdpRequiresDedicatedRealm,
	domain.ErrRealmAlreadyDedicated:          realmAlreadyDedicated,
	domain.ErrRevertWindowExpired:            realmRevertWindowExpired,
	domain.ErrRealmNameUnavailable:           realmNameUnavailable,
	domain.ErrTenantOffboarded:               realmTenantOffboarded,
	domain.ErrOperationInProgress:            realmOperationInProgress,
	domain.ErrOptimisticLockConflict:         realmOptimisticLockConflict,
	domain.ErrUserEmailConflict:              realmUserEmailConflict,
	domain.ErrServiceAccountRotationInFlight: realmServiceAccountRotationInFlight,
	domain.ErrCredentialReplayRevoked:        realmCredentialReplayRevoked,
	domain.ErrCredentialReplayExpired:        realmCredentialReplayExpired,
	domain.ErrKeycloakUnavailable:            realmKeycloakUnavailable,
	domain.ErrConversionFailed:               realmConversionFailed,
	domain.ErrOrgMembershipUnavailable:       realmOrgMembershipUnavailable,
	domain.ErrObjectStoreUnavailable:         realmObjectStoreUnavailable,
	domain.ErrDBUnavailable:                  realmDBUnavailable,
	domain.ErrTokenServiceUnavailable:        realmTokenServiceUnavailable,
	domain.ErrRequestTimeout:                 realmRequestTimeout,
}

// domainErrToProblem converts any error to a *problem.Error for RFC 9457
// rendering at the HTTP adapter boundary. *domain.Error values are mapped via
// the frozen §17 code table. Leaked pgconn errors are classified before the
// generic problem.FromError fallback so they never surface as 500s:
// statement_timeout/lock_timeout → 503 REQUEST_TIMEOUT; connection-class,
// resource-exhaustion, operator-intervention, system errors → 503 DB_UNAVAILABLE.
func domainErrToProblem(err error) *problem.Error {
	var de *domain.Error
	if errors.As(err, &de) {
		t, ok := domainCodeTypes[de.Code]
		if !ok {
			return problem.NewInternalError(err)
		}
		return problem.NewError(t, de.Message)
	}
	if pgcommon.IsQueryCanceled(err) || pgcommon.IsLockNotAvailable(err) {
		return problem.NewError(realmRequestTimeout, "database statement or lock timeout")
	}
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err) || pgcommon.IsPoolClosed(err) {
		return problem.NewError(realmDBUnavailable, "database is unavailable")
	}
	return problem.FromError(err)
}
