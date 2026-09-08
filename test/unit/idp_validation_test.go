package unit_test

import (
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// The §11.6 checklist has 16 checks total; domain.CriticalChecks and domain.WarnChecks must
// partition the checks that actually gate a request (check_3 is informational
// only and appears in neither list), and must never overlap.
func TestCriticalAndWarnChecks_NoOverlap(t *testing.T) {
	seen := make(map[string]string, len(domain.CriticalChecks)+len(domain.WarnChecks))
	for _, c := range domain.CriticalChecks {
		seen[c] = "critical"
	}
	for _, c := range domain.WarnChecks {
		if list, ok := seen[c]; ok {
			t.Errorf("check %q appears in both domain.CriticalChecks and %s", c, list)
		}
		seen[c] = "warn"
	}
}

func TestCriticalChecks_ContainsExpectedIDs(t *testing.T) {
	want := []string{
		domain.CheckConfigSafe,
		domain.CheckSSOURLRegion, domain.CheckSLOURLRegion, domain.CheckCustomerSigningCert,
		domain.CheckACSURLRegion, domain.CheckSLOResponseRegion, domain.CheckEntityIDRegion,
		domain.CheckOIDCIssuerRegion, domain.CheckOIDCRedirectRegion, domain.CheckOIDCAudienceRegion,
		domain.CheckOIDCClientID, domain.CheckOIDCIssuerHTTPS,
	}
	if len(domain.CriticalChecks) != len(want) {
		t.Fatalf("len(domain.CriticalChecks) = %d, want %d", len(domain.CriticalChecks), len(want))
	}
	for i, id := range want {
		if domain.CriticalChecks[i] != id {
			t.Errorf("domain.CriticalChecks[%d] = %q, want %q", i, domain.CriticalChecks[i], id)
		}
	}
}

func TestWarnChecks_ContainsExpectedIDs(t *testing.T) {
	want := []string{
		domain.CheckNameIDFormat, domain.CheckAttributeMapperURN, domain.CheckSignatureAlgorithm, domain.CheckOIDCPKCE,
	}
	if len(domain.WarnChecks) != len(want) {
		t.Fatalf("len(domain.WarnChecks) = %d, want %d", len(domain.WarnChecks), len(want))
	}
	for i, id := range want {
		if domain.WarnChecks[i] != id {
			t.Errorf("domain.WarnChecks[%d] = %q, want %q", i, domain.WarnChecks[i], id)
		}
	}
}

func TestCheckIDConstants_AreDistinct(t *testing.T) {
	ids := []string{
		domain.CheckConfigSafe,
		domain.CheckSSOURLRegion, domain.CheckSLOURLRegion, domain.CheckCustomerSigningCert,
		domain.CheckACSURLRegion, domain.CheckSLOResponseRegion, domain.CheckEntityIDRegion,
		domain.CheckNameIDFormat, domain.CheckAttributeMapperURN, domain.CheckSignatureAlgorithm,
		domain.CheckOIDCIssuerRegion, domain.CheckOIDCRedirectRegion, domain.CheckOIDCAudienceRegion,
		domain.CheckOIDCPKCE, domain.CheckOIDCClientID, domain.CheckOIDCIssuerHTTPS,
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate check ID constant value %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 16 {
		t.Errorf("got %d distinct check IDs, want 16 (§11.6 checklist size)", len(seen))
	}
}
