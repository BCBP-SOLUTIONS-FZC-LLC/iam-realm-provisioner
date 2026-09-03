package unit_test

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func TestTenantRealm_IsOffboarded(t *testing.T) {
	cases := []struct {
		status domain.RealmStatus
		want   bool
	}{
		{domain.RealmStatusProvisioning, false},
		{domain.RealmStatusActive, false},
		{domain.RealmStatusDisabled, false},
		{domain.RealmStatusSuspended, false},
		{domain.RealmStatusExported, true},
		{domain.RealmStatusDeleted, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			r := &domain.TenantRealm{Status: tc.status}
			if got := r.IsOffboarded(); got != tc.want {
				t.Errorf("IsOffboarded() for status %q = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestTenantRealm_IsDedicated(t *testing.T) {
	cases := []struct {
		strategy domain.RealmStrategy
		want     bool
	}{
		{domain.RealmStrategyDedicated, true},
		{domain.RealmStrategyShared, false},
		{domain.RealmStrategy(""), false},
	}

	for _, tc := range cases {
		t.Run(string(tc.strategy), func(t *testing.T) {
			r := &domain.TenantRealm{Strategy: tc.strategy}
			if got := r.IsDedicated(); got != tc.want {
				t.Errorf("IsDedicated() for strategy %q = %v, want %v", tc.strategy, got, tc.want)
			}
		})
	}
}
