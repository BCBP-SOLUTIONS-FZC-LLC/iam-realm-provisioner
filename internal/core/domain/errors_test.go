package domain

import "testing"

func TestErrorStatus(t *testing.T) {
	// Per .claude/api-caching-events.md's §17 error taxonomy table.
	cases := []struct {
		code ErrorCode
		want int
	}{
		{ErrInvalidRequest, 400},
		{ErrInvalidRealmStrategy, 400},
		{ErrInvalidIdpProtocol, 400},
		{ErrUnsupportedRealmSetting, 400},
		{ErrIdpRegionPortabilityFailed, 400},
		{ErrMissingIdempotencyKey, 400},
		{ErrIdpWarningsUnacknowledged, 422},
		{ErrTenantRealmNotFound, 404},
		{ErrIdpConfigNotFound, 404},
		{ErrIdpRequiresDedicatedRealm, 409},
		{ErrRealmAlreadyDedicated, 409},
		{ErrRevertWindowExpired, 409},
		{ErrTenantOffboarded, 409},
		{ErrOperationInProgress, 409},
		{ErrOptimisticLockConflict, 409},
		{ErrKeycloakUnavailable, 502},
		{ErrConversionFailed, 502},
		{ErrOrgMembershipUnavailable, 503},
		{ErrObjectStoreUnavailable, 503},
	}

	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			e := &Error{Code: tc.code}
			if got := e.Status(); got != tc.want {
				t.Errorf("Status() for %q = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

func TestErrorStatus_UnknownCodeDefaultsTo500(t *testing.T) {
	e := &Error{Code: ErrorCode("some_unregistered_code")}
	if got := e.Status(); got != 500 {
		t.Errorf("Status() for unknown code = %d, want 500", got)
	}
}

func TestErrorStatus_EmptyCodeDefaultsTo500(t *testing.T) {
	e := &Error{}
	if got := e.Status(); got != 500 {
		t.Errorf("Status() for empty code = %d, want 500", got)
	}
}

func TestError_ErrorMessage(t *testing.T) {
	e := &Error{Code: ErrTenantRealmNotFound, Message: "tenant realm not found"}
	want := "tenant_realm_not_found: tenant realm not found"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNewError(t *testing.T) {
	e := NewError(ErrKeycloakUnavailable, "boom")
	if e.Code != ErrKeycloakUnavailable {
		t.Errorf("Code = %q, want %q", e.Code, ErrKeycloakUnavailable)
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
	e := NewError(ErrOptimisticLockConflict, "conflict")
	details := map[string]any{"record_version": 3}

	got := e.WithDetails(details)

	if got != e {
		t.Error("WithDetails should return the same *Error for chaining")
	}
	if e.Details["record_version"] != 3 {
		t.Errorf("Details[record_version] = %v, want 3", e.Details["record_version"])
	}
}
