package domain

import "testing"

func TestTenantRealm_IsOffboarded(t *testing.T) {
	cases := []struct {
		status RealmStatus
		want   bool
	}{
		{RealmStatusProvisioning, false},
		{RealmStatusActive, false},
		{RealmStatusDisabled, false},
		{RealmStatusSuspended, false},
		{RealmStatusExported, true},
		{RealmStatusDeleted, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			r := &TenantRealm{Status: tc.status}
			if got := r.IsOffboarded(); got != tc.want {
				t.Errorf("IsOffboarded() for status %q = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestTenantRealm_IsDedicated(t *testing.T) {
	cases := []struct {
		strategy RealmStrategy
		want     bool
	}{
		{RealmStrategyDedicated, true},
		{RealmStrategyShared, false},
		{RealmStrategy(""), false},
	}

	for _, tc := range cases {
		t.Run(string(tc.strategy), func(t *testing.T) {
			r := &TenantRealm{Strategy: tc.strategy}
			if got := r.IsDedicated(); got != tc.want {
				t.Errorf("IsDedicated() for strategy %q = %v, want %v", tc.strategy, got, tc.want)
			}
		})
	}
}
