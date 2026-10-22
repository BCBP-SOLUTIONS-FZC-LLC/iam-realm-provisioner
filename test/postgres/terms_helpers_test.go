//go:build integration

package postgres_test

import (
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// validTerms is a fresh, valid RP-1/RP-2 terms_acceptance.
func validTerms() *domain.TermsAcceptance {
	return &domain.TermsAcceptance{
		Version: "2026-09", AcceptedAt: time.Now().Add(-time.Minute).UTC(),
		IPAddress: "203.0.113.7", UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6)",
	}
}
