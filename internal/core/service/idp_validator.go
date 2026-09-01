package service

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// regionSubstrings are AWS-region tokens that must never appear in a
// customer-facing IdP URL (§11.6) — pinning the platform to a region would
// block a future cross-region migration.
var regionSubstrings = []string{
	"ap-south-1", "us-east-1", "us-west-2", "eu-west-1", "eu-central-1",
	"ap-southeast-1", "ap-southeast-2", "ap-northeast-1",
}

// regionNeutralHost is the one hostname every SAML/OIDC endpoint pointing
// at this platform must use (§11.6 checks 4-6, 10-11).
const regionNeutralHost = "auth.bcbpsolutions.com"

// carriesRegion reports whether s contains an AWS region substring.
func carriesRegion(s string) bool {
	lower := strings.ToLower(s)
	for _, r := range regionSubstrings {
		if strings.Contains(lower, r) {
			return true
		}
	}
	return false
}

// isRegionNeutralAuthURL reports whether rawURL points at the
// region-neutral auth host under /realms/<realm>/... .
func isRegionNeutralAuthURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Host == regionNeutralHost
}

// IdpValidationResult is the outcome of running the §11.6 checklist once.
type IdpValidationResult struct {
	Outcome domain.ValidationOutcome
	Checks  map[string]domain.CheckOutcome
	Failed  []string // CRITICAL check IDs that failed
	Warned  []string // WARN check IDs that failed
}

// ValidatorVersion is the current §11.6 checklist version, recorded on
// every keycloak_idp_config_validations row.
const ValidatorVersion = 1

// ValidateIdpConfig runs the §11.6 Region-Agnostic IdP Configuration
// Checklist against cfg. CRITICAL failures (checks 1-2, 4-6, 10-12) block
// with 400; WARN failures (7-9, 13) require a recorded acknowledgement.
func ValidateIdpConfig(protocol domain.IdpProtocol, cfg map[string]any, acked map[string]bool) IdpValidationResult {
	checks := map[string]domain.CheckOutcome{}
	var failed, warned []string

	record := func(id string, pass bool, critical bool) {
		if pass {
			checks[id] = domain.CheckPass
			return
		}
		if critical {
			checks[id] = domain.CheckFail
			failed = append(failed, id)
			return
		}
		if acked[id] {
			checks[id] = domain.CheckPass
			return
		}
		checks[id] = domain.CheckWarn
		warned = append(warned, id)
	}

	str := func(key string) string {
		if v, ok := cfg[key].(string); ok {
			return v
		}
		return ""
	}

	if protocol == domain.IdpProtocolSAML {
		ssoURL := str("sso_url")
		sloURL := str("slo_url")
		acsURL := str("acs_url")
		entityID := str("entity_id")
		signingCert := str("signing_cert")
		nameIDFormat := str("nameid_format")
		sigAlg := str("signature_algorithm")

		record(domain.CheckSSOURLRegion, ssoURL == "" || !carriesRegion(ssoURL), true)
		record(domain.CheckSLOURLRegion, sloURL == "" || !carriesRegion(sloURL), true)
		record(domain.CheckCustomerSigningCert, signingCert != "", true)
		record(domain.CheckACSURLRegion, acsURL == "" || isRegionNeutralAuthURL(acsURL), true)
		record(domain.CheckSLOResponseRegion, sloURL == "" || !carriesRegion(sloURL), true)
		record(domain.CheckEntityIDRegion, entityID == "" || !carriesRegion(entityID), true)
		record(domain.CheckNameIDFormat, nameIDFormat != "transient", false)
		record(domain.CheckAttributeMapperURN, !mapperURNsCarryRegion(cfg), false)
		record(domain.CheckSignatureAlgorithm, sigAlg == "" || isStrongSignatureAlgorithm(sigAlg), false)
	} else {
		issuer := str("issuer")
		redirectURI := str("redirect_uri")
		audience := str("audience")
		pkce := str("pkce_method")

		record(domain.CheckOIDCIssuerRegion, issuer == "" || !carriesRegion(issuer), true)
		record(domain.CheckOIDCRedirectRegion, redirectURI == "" || !carriesRegion(redirectURI), true)
		record(domain.CheckOIDCAudienceRegion, audience == "" || !carriesRegion(audience), true)
		record(domain.CheckOIDCPKCE, pkce == "S256", false)
	}

	outcome := domain.ValidationOutcomePass
	if len(failed) > 0 {
		outcome = domain.ValidationOutcomeFailing
	} else if len(warned) > 0 {
		outcome = domain.ValidationOutcomeWarningsOnly
	}
	return IdpValidationResult{Outcome: outcome, Checks: checks, Failed: failed, Warned: warned}
}

func mapperURNsCarryRegion(cfg map[string]any) bool {
	mappers, ok := cfg["attribute_mappers"].([]any)
	if !ok {
		return false
	}
	for _, m := range mappers {
		if s, ok := m.(string); ok && carriesRegion(s) {
			return true
		}
	}
	return false
}

var strongSigAlgPattern = regexp.MustCompile(`^RSA-SHA(256|384|512)$`)

func isStrongSignatureAlgorithm(alg string) bool {
	return strongSigAlgPattern.MatchString(alg)
}
