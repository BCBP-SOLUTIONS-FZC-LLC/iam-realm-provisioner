package unit_test

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func TestErrorStatus(t *testing.T) {
	// Per .claude/api-caching-events.md's §17 error taxonomy table.
	cases := []struct {
		code domain.ErrorCode
		want int
	}{
		{domain.ErrInvalidRequest, 400},
		{domain.ErrInvalidRealmStrategy, 400},
		{domain.ErrInvalidIdpProtocol, 400},
		{domain.ErrUnsupportedRealmSetting, 400},
		{domain.ErrIdpRegionPortabilityFailed, 400},
		{domain.ErrMissingIdempotencyKey, 400},
		{domain.ErrIdpWarningsUnacknowledged, 422},
		{domain.ErrTenantRealmNotFound, 404},
		{domain.ErrIdpConfigNotFound, 404},
		{domain.ErrIdpRequiresDedicatedRealm, 409},
		{domain.ErrRealmAlreadyDedicated, 409},
		{domain.ErrRevertWindowExpired, 409},
		{domain.ErrTenantOffboarded, 409},
		{domain.ErrOperationInProgress, 409},
		{domain.ErrOptimisticLockConflict, 409},
		{domain.ErrKeycloakUnavailable, 502},
		{domain.ErrConversionFailed, 502},
		{domain.ErrOrgMembershipUnavailable, 503},
		{domain.ErrObjectStoreUnavailable, 503},
		{domain.ErrDBUnavailable, 503},
	}

	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			e := &domain.Error{Code: tc.code}
			if got := e.Status(); got != tc.want {
				t.Errorf("Status() for %q = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

func TestErrorStatus_UnknownCodeDefaultsTo500(t *testing.T) {
	e := &domain.Error{Code: domain.ErrorCode("some_unregistered_code")}
	if got := e.Status(); got != 500 {
		t.Errorf("Status() for unknown code = %d, want 500", got)
	}
}

func TestErrorStatus_EmptyCodeDefaultsTo500(t *testing.T) {
	e := &domain.Error{}
	if got := e.Status(); got != 500 {
		t.Errorf("Status() for empty code = %d, want 500", got)
	}
}

func TestError_ErrorMessage(t *testing.T) {
	e := &domain.Error{Code: domain.ErrTenantRealmNotFound, Message: "tenant realm not found"}
	want := "tenant_realm_not_found: tenant realm not found"
	if got := e.Error(); got != want {
		t.Errorf("domain.Error() = %q, want %q", got, want)
	}
}

func TestNewError(t *testing.T) {
	e := domain.NewError(domain.ErrKeycloakUnavailable, "boom")
	if e.Code != domain.ErrKeycloakUnavailable {
		t.Errorf("Code = %q, want %q", e.Code, domain.ErrKeycloakUnavailable)
	}
	if e.Message != "boom" {
		t.Errorf("Message = %q, want %q", e.Message, "boom")
	}
	if e.Details != nil {
		t.Errorf("Details = %v, want nil", e.Details)
	}
	if e.Status() != 502 {
		t.Errorf("Status() = %d, want 502", e.Status())
	}
}

func TestError_WithDetails(t *testing.T) {
	e := domain.NewError(domain.ErrOptimisticLockConflict, "conflict")
	details := map[string]any{"record_version": 3}

	got := e.WithDetails(details)

	if got != e {
		t.Error("WithDetails should return the same *domain.Error for chaining")
	}
	if e.Details["record_version"] != 3 {
		t.Errorf("Details[record_version] = %v, want 3", e.Details["record_version"])
	}
}
