package service

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func TestValidateIdpConfig_SAML_RegionPinnedURL_Fails(t *testing.T) {
	result := ValidateIdpConfig(domain.IdpProtocolSAML, map[string]any{
		"sso_url":       "https://idp.example.com/sso/ap-south-1/saml",
		"acs_url":       "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"entity_id":     "urn:acme:sp",
		"signing_cert":  "-----BEGIN CERTIFICATE-----...",
		"nameid_format": "persistent",
	}, nil)
	assert.Equal(t, domain.ValidationOutcomeFailing, result.Outcome)
	assert.Contains(t, result.Failed, domain.CheckSSOURLRegion)
}

func TestValidateIdpConfig_SAML_RegionNeutral_Passes(t *testing.T) {
	result := ValidateIdpConfig(domain.IdpProtocolSAML, map[string]any{
		"sso_url":             "https://idp.example.com/sso/saml",
		"slo_url":             "https://idp.example.com/slo/saml",
		"acs_url":             "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"entity_id":           "urn:acme:sp",
		"signing_cert":        "-----BEGIN CERTIFICATE-----...",
		"nameid_format":       "persistent",
		"signature_algorithm": "RSA-SHA256",
	}, nil)
	assert.Equal(t, domain.ValidationOutcomePass, result.Outcome)
	assert.Empty(t, result.Failed)
	assert.Empty(t, result.Warned)
}

func TestValidateIdpConfig_SAML_WarnRequiresAck(t *testing.T) {
	cfg := map[string]any{
		"sso_url":       "https://idp.example.com/sso/saml",
		"acs_url":       "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"entity_id":     "urn:acme:sp",
		"signing_cert":  "-----BEGIN CERTIFICATE-----...",
		"nameid_format": "transient", // WARN — check_7
	}
	unacked := ValidateIdpConfig(domain.IdpProtocolSAML, cfg, nil)
	assert.Equal(t, domain.ValidationOutcomeWarningsOnly, unacked.Outcome)
	assert.Contains(t, unacked.Warned, domain.CheckNameIDFormat)

	acked := ValidateIdpConfig(domain.IdpProtocolSAML, cfg, map[string]bool{domain.CheckNameIDFormat: true})
	assert.Equal(t, domain.ValidationOutcomePass, acked.Outcome)
}

func TestValidateIdpConfig_OIDC_RegionPinnedIssuer_Fails(t *testing.T) {
	result := ValidateIdpConfig(domain.IdpProtocolOIDC, map[string]any{
		"issuer":       "https://login.example.com/us-east-1/oauth2",
		"redirect_uri": "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"audience":     "acme-client",
		"pkce_method":  "S256",
	}, nil)
	assert.Equal(t, domain.ValidationOutcomeFailing, result.Outcome)
	assert.Contains(t, result.Failed, domain.CheckOIDCIssuerRegion)
}

func TestValidateIdpConfig_OIDC_MissingPKCE_Warns(t *testing.T) {
	result := ValidateIdpConfig(domain.IdpProtocolOIDC, map[string]any{
		"issuer":       "https://login.example.com/oauth2",
		"redirect_uri": "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"audience":     "acme-client",
		"client_id":    "acme-client-id",
	}, nil)
	assert.Equal(t, domain.ValidationOutcomeWarningsOnly, result.Outcome)
	assert.Contains(t, result.Warned, domain.CheckOIDCPKCE)
}

// validBaselineSAMLConfig is a config that passes every §11.6 SAML check —
// the per-check tests below mutate exactly one field off this baseline so
// each failure can be attributed to a single check.
func validBaselineSAMLConfig() map[string]any {
	return map[string]any{
		"sso_url":             "https://idp.example.com/sso/saml",
		"slo_url":             "https://idp.example.com/slo/saml",
		"acs_url":             "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"entity_id":           "urn:acme:sp",
		"signing_cert":        "-----BEGIN CERTIFICATE-----...",
		"nameid_format":       "persistent",
		"signature_algorithm": "RSA-SHA256",
	}
}

func validBaselineOIDCConfig() map[string]any {
	return map[string]any{
		"issuer":       "https://login.example.com/oauth2",
		"redirect_uri": "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"audience":     "acme-client",
		"pkce_method":  "S256",
		"client_id":    "acme-client-id",
	}
}

// Every §11.6 CRITICAL check (SAML: checks 1,2,3,4,5,6; OIDC: checks
// 10,11,12) blocks with a Failing outcome, independently of the others.
func TestValidateIdpConfig_CriticalChecks_EachBlocksIndependently(t *testing.T) {
	cases := []struct {
		name      string
		protocol  domain.IdpProtocol
		mutate    func(cfg map[string]any)
		wantCheck string
	}{
		{"saml_sso_url_region", domain.IdpProtocolSAML, func(c map[string]any) { c["sso_url"] = "https://idp.example.com/ap-south-1/sso" }, domain.CheckSSOURLRegion},
		{"saml_slo_url_region", domain.IdpProtocolSAML, func(c map[string]any) { c["slo_url"] = "https://idp.example.com/us-east-1/slo" }, domain.CheckSLOURLRegion},
		{"saml_missing_signing_cert", domain.IdpProtocolSAML, func(c map[string]any) { c["signing_cert"] = "" }, domain.CheckCustomerSigningCert},
		{"saml_acs_url_wrong_host", domain.IdpProtocolSAML, func(c map[string]any) { c["acs_url"] = "https://idp.example.com/realms/acme/broker/idp/endpoint" }, domain.CheckACSURLRegion},
		{"saml_acs_url_unparseable", domain.IdpProtocolSAML, func(c map[string]any) { c["acs_url"] = "://not-a-valid-url" }, domain.CheckACSURLRegion},
		{"saml_slo_response_region", domain.IdpProtocolSAML, func(c map[string]any) { c["slo_url"] = "https://idp.example.com/eu-west-1/slo" }, domain.CheckSLOResponseRegion},
		{"saml_entity_id_region", domain.IdpProtocolSAML, func(c map[string]any) { c["entity_id"] = "urn:acme:ap-southeast-1:sp" }, domain.CheckEntityIDRegion},
		{"oidc_issuer_region", domain.IdpProtocolOIDC, func(c map[string]any) { c["issuer"] = "https://login.example.com/us-east-1/oauth2" }, domain.CheckOIDCIssuerRegion},
		{"oidc_redirect_region", domain.IdpProtocolOIDC, func(c map[string]any) { c["redirect_uri"] = "https://login.example.com/eu-central-1/callback" }, domain.CheckOIDCRedirectRegion},
		{"oidc_audience_region", domain.IdpProtocolOIDC, func(c map[string]any) { c["audience"] = "acme-ap-northeast-1-client" }, domain.CheckOIDCAudienceRegion},
		{"saml_injection_in_field", domain.IdpProtocolSAML, func(c map[string]any) { c["signing_cert"] = "<script>alert(1)</script>" }, domain.CheckConfigSafe},
		{"oidc_injection_in_field", domain.IdpProtocolOIDC, func(c map[string]any) { c["client_id"] = "acme<img src=x>" }, domain.CheckConfigSafe},
		{"oidc_issuer_not_https", domain.IdpProtocolOIDC, func(c map[string]any) { c["issuer"] = "http://login.example.com/oauth2" }, domain.CheckOIDCIssuerHTTPS},
		{"oidc_issuer_empty", domain.IdpProtocolOIDC, func(c map[string]any) { c["issuer"] = "" }, domain.CheckOIDCIssuerHTTPS},
		{"oidc_issuer_unparseable", domain.IdpProtocolOIDC, func(c map[string]any) { c["issuer"] = "://not-a-valid-url" }, domain.CheckOIDCIssuerHTTPS},
		{"oidc_issuer_https_no_host", domain.IdpProtocolOIDC, func(c map[string]any) { c["issuer"] = "https:///no-host" }, domain.CheckOIDCIssuerHTTPS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg map[string]any
			if tc.protocol == domain.IdpProtocolSAML {
				cfg = validBaselineSAMLConfig()
			} else {
				cfg = validBaselineOIDCConfig()
			}
			tc.mutate(cfg)
			result := ValidateIdpConfig(tc.protocol, cfg, nil)
			assert.Equal(t, domain.ValidationOutcomeFailing, result.Outcome)
			assert.Contains(t, result.Failed, tc.wantCheck)
			assert.Equal(t, domain.CheckFail, result.Checks[tc.wantCheck])
		})
	}
}

// Every §11.6 WARN check (SAML: 7,8,9; OIDC: 13) requires a recorded
// acknowledgement; without one it downgrades the outcome to
// warnings_only rather than blocking, and the specific ack clears it.
func TestValidateIdpConfig_WarnChecks_RequireAcknowledgement(t *testing.T) {
	cases := []struct {
		name      string
		protocol  domain.IdpProtocol
		mutate    func(cfg map[string]any)
		wantCheck string
	}{
		{"saml_nameid_transient", domain.IdpProtocolSAML, func(c map[string]any) { c["nameid_format"] = "transient" }, domain.CheckNameIDFormat},
		{"saml_attribute_mapper_urn_region", domain.IdpProtocolSAML, func(c map[string]any) {
			c["attribute_mappers"] = []any{"urn:oid:ap-south-1:employeeID"}
		}, domain.CheckAttributeMapperURN},
		{"saml_weak_signature_algorithm", domain.IdpProtocolSAML, func(c map[string]any) { c["signature_algorithm"] = "RSA-SHA1" }, domain.CheckSignatureAlgorithm},
		{"oidc_missing_pkce", domain.IdpProtocolOIDC, func(c map[string]any) { c["pkce_method"] = "" }, domain.CheckOIDCPKCE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg map[string]any
			if tc.protocol == domain.IdpProtocolSAML {
				cfg = validBaselineSAMLConfig()
			} else {
				cfg = validBaselineOIDCConfig()
			}
			tc.mutate(cfg)

			unacked := ValidateIdpConfig(tc.protocol, cfg, nil)
			assert.Equal(t, domain.ValidationOutcomeWarningsOnly, unacked.Outcome, "unacknowledged WARN must downgrade, not block")
			assert.Contains(t, unacked.Warned, tc.wantCheck)
			assert.Equal(t, domain.CheckWarn, unacked.Checks[tc.wantCheck])

			acked := ValidateIdpConfig(tc.protocol, cfg, map[string]bool{tc.wantCheck: true})
			assert.Equal(t, domain.ValidationOutcomePass, acked.Outcome, "the matching acknowledgement clears the WARN")
			assert.Empty(t, acked.Warned)
		})
	}
}

// check 5 (CheckSLOResponseRegion) examines slo_response_url, which
// defaults to slo_url when the caller never supplies a distinct value —
// the overwhelming common case, since a SAML SingleLogoutService's
// ResponseLocation defaults to its Location when unset. A caller that
// never sets slo_response_url therefore fails both checks 2 and 5
// simultaneously off the one region-pinned slo_url.
func TestValidateIdpConfig_SLOURLRegion_NoDistinctResponseURL_FailsBothDependentChecks(t *testing.T) {
	cfg := validBaselineSAMLConfig()
	cfg["slo_url"] = "https://idp.example.com/ap-south-1/slo"
	result := ValidateIdpConfig(domain.IdpProtocolSAML, cfg, nil)
	assert.Equal(t, domain.ValidationOutcomeFailing, result.Outcome)
	assert.Contains(t, result.Failed, domain.CheckSLOURLRegion)
	assert.Contains(t, result.Failed, domain.CheckSLOResponseRegion)
}

// CRITICAL: when a caller DOES supply a distinct slo_response_url, check 5
// must examine THAT value independently of slo_url — not silently re-check
// slo_url under a different check ID (the bug this test guards against: a
// region-pinned slo_response_url must fail check 5 even when slo_url
// itself is perfectly region-neutral).
func TestValidateIdpConfig_DistinctSLOResponseURL_ValidatedIndependently(t *testing.T) {
	cfg := validBaselineSAMLConfig()
	cfg["slo_url"] = "https://idp.example.com/slo/saml" // region-neutral, must pass check 2
	cfg["slo_response_url"] = "https://idp.example.com/ap-south-1/slo-response"
	result := ValidateIdpConfig(domain.IdpProtocolSAML, cfg, nil)
	assert.Equal(t, domain.ValidationOutcomeFailing, result.Outcome)
	assert.NotContains(t, result.Failed, domain.CheckSLOURLRegion, "slo_url itself is region-neutral and must pass check 2")
	assert.Contains(t, result.Failed, domain.CheckSLOResponseRegion, "a region-pinned slo_response_url must fail check 5 even though slo_url passes")
}

// Optional fields left empty are treated as pass, not fail — an IdP config
// that simply omits SLO (no single-logout support) and signature_algorithm
// must not be blocked or warned.
func TestValidateIdpConfig_SAML_EmptyOptionalFields_TreatedAsPass(t *testing.T) {
	result := ValidateIdpConfig(domain.IdpProtocolSAML, map[string]any{
		"sso_url":      "https://idp.example.com/sso/saml",
		"acs_url":      "https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint",
		"entity_id":    "urn:acme:sp",
		"signing_cert": "-----BEGIN CERTIFICATE-----...",
		// slo_url, nameid_format, signature_algorithm all absent.
	}, nil)
	assert.Equal(t, domain.ValidationOutcomePass, result.Outcome)
	assert.Empty(t, result.Failed)
	assert.Empty(t, result.Warned)
}

func TestMapperURNsCarryRegion_NoMappersKey(t *testing.T) {
	assert.False(t, mapperURNsCarryRegion(map[string]any{}))
}

func TestMapperURNsCarryRegion_WrongType_TreatedAsAbsent(t *testing.T) {
	assert.False(t, mapperURNsCarryRegion(map[string]any{"attribute_mappers": "not-a-list"}))
}

func TestMapperURNsCarryRegion_NonStringElementsIgnored(t *testing.T) {
	assert.False(t, mapperURNsCarryRegion(map[string]any{"attribute_mappers": []any{42, true, nil}}))
}

func TestMapperURNsCarryRegion_RegionCarryingURN(t *testing.T) {
	assert.True(t, mapperURNsCarryRegion(map[string]any{
		"attribute_mappers": []any{"urn:oid:employeeID", "urn:oid:ap-south-1:department"},
	}))
}

func TestIsRegionNeutralAuthURL_ParseError(t *testing.T) {
	assert.False(t, isRegionNeutralAuthURL("://not-a-valid-url"))
}

func TestIsRegionNeutralAuthURL_WrongHost(t *testing.T) {
	assert.False(t, isRegionNeutralAuthURL("https://idp.example.com/realms/acme/broker/idp/endpoint"))
}

func TestIsRegionNeutralAuthURL_CorrectHost(t *testing.T) {
	assert.True(t, isRegionNeutralAuthURL("https://auth.bcbpsolutions.com/realms/acme/broker/idp/endpoint"))
}
