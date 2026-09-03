package domain

// CheckOutcome is the per-check result recorded in
// `keycloak_idp_config_validations.checks` (§4.3).
type CheckOutcome string

// CheckOutcome values.
const (
	CheckPass CheckOutcome = "pass"
	CheckWarn CheckOutcome = "warn"
	CheckFail CheckOutcome = "fail"
)

// CheckSeverity classifies a §11.6 checklist item.
type CheckSeverity string

// CheckSeverity values.
const (
	SeverityCritical CheckSeverity = "critical" // blocks with 400
	SeverityWarn     CheckSeverity = "warn"     // allowed with recorded ack
)

// §11.6 Region-Agnostic IdP Configuration Checklist check IDs. CRITICAL
// checks (1-6, 10-12) block with 400 idp_region_portability_failed;
// WARN checks (7-9, 13) allow with a recorded acknowledgement.
const (
	CheckSSOURLRegion        = "check_1_sso_url_region"
	CheckSLOURLRegion        = "check_2_slo_url_region"
	CheckCustomerSigningCert = "check_3_customer_signing_cert"
	CheckACSURLRegion        = "check_4_acs_url_region"
	CheckSLOResponseRegion   = "check_5_slo_response_region"
	CheckEntityIDRegion      = "check_6_entity_id_region"
	CheckNameIDFormat        = "check_7_nameid_format"
	CheckAttributeMapperURN  = "check_8_attribute_mapper_urn"
	CheckSignatureAlgorithm  = "check_9_signature_algorithm"
	CheckOIDCIssuerRegion    = "check_10_oidc_issuer_region"
	CheckOIDCRedirectRegion  = "check_11_oidc_redirect_region" //nolint:gosec // G101 false positive: a §11.6 checklist ID constant, not a credential
	CheckOIDCAudienceRegion  = "check_12_oidc_audience_region"
	CheckOIDCPKCE            = "check_13_oidc_pkce"
)

// CriticalChecks lists every §11.6 check whose failure blocks with 400.
var CriticalChecks = []string{
	CheckSSOURLRegion, CheckSLOURLRegion, CheckCustomerSigningCert,
	CheckACSURLRegion, CheckSLOResponseRegion, CheckEntityIDRegion,
	CheckOIDCIssuerRegion, CheckOIDCRedirectRegion, CheckOIDCAudienceRegion,
}

// WarnChecks lists every §11.6 check whose failure requires a recorded
// admin acknowledgement rather than blocking.
var WarnChecks = []string{
	CheckNameIDFormat, CheckAttributeMapperURN, CheckSignatureAlgorithm, CheckOIDCPKCE,
}
