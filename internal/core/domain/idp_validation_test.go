package domain

import "testing"

// The §11.6 checklist has 13 checks total; CriticalChecks and WarnChecks must
// partition the checks that actually gate a request (check_3 is informational
// only and appears in neither list), and must never overlap.
func TestCriticalAndWarnChecks_NoOverlap(t *testing.T) {
	seen := make(map[string]string, len(CriticalChecks)+len(WarnChecks))
	for _, c := range CriticalChecks {
		seen[c] = "critical"
	}
	for _, c := range WarnChecks {
		if list, ok := seen[c]; ok {
			t.Errorf("check %q appears in both CriticalChecks and %s", c, list)
		}
		seen[c] = "warn"
	}
}

func TestCriticalChecks_ContainsExpectedIDs(t *testing.T) {
	want := []string{
		CheckSSOURLRegion, CheckSLOURLRegion, CheckCustomerSigningCert,
		CheckACSURLRegion, CheckSLOResponseRegion, CheckEntityIDRegion,
		CheckOIDCIssuerRegion, CheckOIDCRedirectRegion, CheckOIDCAudienceRegion,
	}
	if len(CriticalChecks) != len(want) {
		t.Fatalf("len(CriticalChecks) = %d, want %d", len(CriticalChecks), len(want))
	}
	for i, id := range want {
		if CriticalChecks[i] != id {
			t.Errorf("CriticalChecks[%d] = %q, want %q", i, CriticalChecks[i], id)
		}
	}
}

func TestWarnChecks_ContainsExpectedIDs(t *testing.T) {
	want := []string{
		CheckNameIDFormat, CheckAttributeMapperURN, CheckSignatureAlgorithm, CheckOIDCPKCE,
	}
	if len(WarnChecks) != len(want) {
		t.Fatalf("len(WarnChecks) = %d, want %d", len(WarnChecks), len(want))
	}
	for i, id := range want {
		if WarnChecks[i] != id {
			t.Errorf("WarnChecks[%d] = %q, want %q", i, WarnChecks[i], id)
		}
	}
}

func TestCheckIDConstants_AreDistinct(t *testing.T) {
	ids := []string{
		CheckSSOURLRegion, CheckSLOURLRegion, CheckCustomerSigningCert,
		CheckACSURLRegion, CheckSLOResponseRegion, CheckEntityIDRegion,
		CheckNameIDFormat, CheckAttributeMapperURN, CheckSignatureAlgorithm,
		CheckOIDCIssuerRegion, CheckOIDCRedirectRegion, CheckOIDCAudienceRegion,
		CheckOIDCPKCE,
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate check ID constant value %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 13 {
		t.Errorf("got %d distinct check IDs, want 13 (§11.6 checklist size)", len(seen))
	}
}
