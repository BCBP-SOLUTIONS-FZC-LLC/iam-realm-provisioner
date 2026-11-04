package http

import (
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestDomainErrToProblem_FullTaxonomy exercises every §17 ErrorCode in the
// domainCodeTypes map, confirming the HTTP status and UPPER_SNAKE_CASE
// errorCode for each.
func TestDomainErrToProblem_FullTaxonomy(t *testing.T) {
	tests := []struct {
		code       domain.ErrorCode
		wantStatus int
		wantCode   string
	}{
		{domain.ErrInvalidRequest, http.StatusBadRequest, "INVALID_REQUEST"},
		{domain.ErrInvalidRealmStrategy, http.StatusBadRequest, "INVALID_REALM_STRATEGY"},
		{domain.ErrInvalidIdpProtocol, http.StatusBadRequest, "INVALID_IDP_PROTOCOL"},
		{domain.ErrUnsupportedRealmSetting, http.StatusBadRequest, "UNSUPPORTED_REALM_SETTING"},
		{domain.ErrIdpRegionPortabilityFailed, http.StatusBadRequest, "IDP_REGION_PORTABILITY_FAILED"},
		{domain.ErrMissingIdempotencyKey, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY"},
		{domain.ErrIdpWarningsUnacknowledged, http.StatusUnprocessableEntity, "IDP_WARNINGS_UNACKNOWLEDGED"},
		{domain.ErrServiceAccountNotProvisioned, http.StatusUnprocessableEntity, "SERVICE_ACCOUNT_NOT_PROVISIONED"},
		{domain.ErrTenantRealmNotFound, http.StatusNotFound, "TENANT_REALM_NOT_FOUND"},
		{domain.ErrIdpConfigNotFound, http.StatusNotFound, "IDP_CONFIG_NOT_FOUND"},
		{domain.ErrKeycloakUserNotFound, http.StatusNotFound, "KEYCLOAK_USER_NOT_FOUND"},
		{domain.ErrIdpRequiresDedicatedRealm, http.StatusConflict, "IDP_REQUIRES_DEDICATED_REALM"},
		{domain.ErrRealmAlreadyDedicated, http.StatusConflict, "REALM_ALREADY_DEDICATED"},
		{domain.ErrRevertWindowExpired, http.StatusConflict, "REVERT_WINDOW_EXPIRED"},
		{domain.ErrTenantOffboarded, http.StatusConflict, "TENANT_OFFBOARDED"},
		{domain.ErrOperationInProgress, http.StatusConflict, "OPERATION_IN_PROGRESS"},
		{domain.ErrOptimisticLockConflict, http.StatusConflict, "OPTIMISTIC_LOCK_CONFLICT"},
		{domain.ErrUserEmailConflict, http.StatusConflict, "EMAIL_ALREADY_REGISTERED"},
		{domain.ErrServiceAccountRotationInFlight, http.StatusConflict, "SERVICE_ACCOUNT_ROTATION_IN_FLIGHT"},
		{domain.ErrCredentialReplayRevoked, http.StatusConflict, "CREDENTIAL_REPLAY_REVOKED"},
		{domain.ErrCredentialReplayExpired, http.StatusConflict, "CREDENTIAL_REPLAY_EXPIRED"},
		{domain.ErrKeycloakUnavailable, http.StatusBadGateway, "KEYCLOAK_UNAVAILABLE"},
		{domain.ErrConversionFailed, http.StatusBadGateway, "CONVERSION_FAILED"},
		{domain.ErrOrgMembershipUnavailable, http.StatusServiceUnavailable, "ORG_MEMBERSHIP_UNAVAILABLE"},
		{domain.ErrObjectStoreUnavailable, http.StatusServiceUnavailable, "OBJECT_STORE_UNAVAILABLE"},
		{domain.ErrDBUnavailable, http.StatusServiceUnavailable, "DB_UNAVAILABLE"},
		{domain.ErrTokenServiceUnavailable, http.StatusServiceUnavailable, "TOKEN_SERVICE_UNAVAILABLE"},
		{domain.ErrRequestTimeout, http.StatusServiceUnavailable, "REQUEST_TIMEOUT"},
	}
	for _, tc := range tests {
		t.Run(string(tc.code), func(t *testing.T) {
			pe := domainErrToProblem(domain.NewError(tc.code, "boom"))
			assert.Equal(t, tc.wantStatus, pe.Status(), "HTTP status for %s", tc.code)
			assert.Equal(t, tc.wantCode, pe.ErrorCode(), "errorCode for %s", tc.code)
		})
	}
}

func TestDomainErrToProblem_UnclassifiedFallsBackTo500(t *testing.T) {
	pe := domainErrToProblem(assert.AnError)
	assert.Equal(t, http.StatusInternalServerError, pe.Status())
	assert.Equal(t, "INTERNAL_ERROR", pe.ErrorCode())
}

func TestDomainErrToProblem_DBConnectivitySQLState_Returns503DBUnavailable(t *testing.T) {
	for _, code := range []string{"08006", "08001", "08004"} {
		t.Run(code, func(t *testing.T) {
			pe := domainErrToProblem(&pgconn.PgError{Code: code})
			assert.Equal(t, http.StatusServiceUnavailable, pe.Status())
			assert.Equal(t, "DB_UNAVAILABLE", pe.ErrorCode())
		})
	}
}

func TestDomainErrToProblem_DBResourceExhaustion_Returns503DBUnavailable(t *testing.T) {
	pe := domainErrToProblem(&pgconn.PgError{Code: "53300"})
	assert.Equal(t, http.StatusServiceUnavailable, pe.Status())
	assert.Equal(t, "DB_UNAVAILABLE", pe.ErrorCode())
}

func TestDomainErrToProblem_OperatorOrSystemError_Returns503DBUnavailable(t *testing.T) {
	for _, code := range []string{"57P01", "58030"} {
		t.Run(code, func(t *testing.T) {
			pe := domainErrToProblem(&pgconn.PgError{Code: code})
			assert.Equal(t, http.StatusServiceUnavailable, pe.Status())
			assert.Equal(t, "DB_UNAVAILABLE", pe.ErrorCode())
		})
	}
}

func TestDomainErrToProblem_ConstraintViolation_Returns500(t *testing.T) {
	pe := domainErrToProblem(&pgconn.PgError{Code: "23505"})
	assert.Equal(t, http.StatusInternalServerError, pe.Status())
	assert.Equal(t, "INTERNAL_ERROR", pe.ErrorCode())
}

// A statement_timeout (57014) or lock_timeout (55P03) that leaked past the
// service layer is a timed-out request, not a database outage.
func TestDomainErrToProblem_StatementOrLockTimeout_Returns503RequestTimeout(t *testing.T) {
	for _, code := range []string{"57014", "55P03"} {
		t.Run(code, func(t *testing.T) {
			pe := domainErrToProblem(&pgconn.PgError{Code: code})
			assert.Equal(t, http.StatusServiceUnavailable, pe.Status())
			assert.Equal(t, "REQUEST_TIMEOUT", pe.ErrorCode())
		})
	}
}

// TestDomainErrToProblem_MessagePropagated verifies the domain error's
// message flows into the RFC 9457 detail field.
func TestDomainErrToProblem_MessagePropagated(t *testing.T) {
	pe := domainErrToProblem(domain.NewError(domain.ErrTenantRealmNotFound, "realm abc not found"))
	assert.Equal(t, "realm abc not found", pe.Detail)
}
