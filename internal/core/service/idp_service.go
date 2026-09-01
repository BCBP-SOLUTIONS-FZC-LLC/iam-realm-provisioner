package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// IdpService implements RP-6/RP-7/RP-13 (§8.4/8.5, RP-INV-4 — the §11.6
// gate).
type IdpService struct {
	realms      port.TenantRealmRepository
	idpConfigs  port.IdpConfigRepository
	validations port.IdpValidationRepository
	openbao     port.OpenBaoClient
	keycloak    port.KeycloakAdmin
	log         port.SlogStyleLogger
}

// NewIdpService wires IdpService's dependencies.
func NewIdpService(
	realms port.TenantRealmRepository,
	idpConfigs port.IdpConfigRepository,
	validations port.IdpValidationRepository,
	openbao port.OpenBaoClient,
	keycloak port.KeycloakAdmin,
) *IdpService {
	return &IdpService{realms: realms, idpConfigs: idpConfigs, validations: validations, openbao: openbao, keycloak: keycloak}
}

// WithLogger attaches a logger and returns s for chaining.
func (s *IdpService) WithLogger(log port.Logger) *IdpService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// ConfigureIdpRequest is the RP-6 request body (§5.2.1).
type ConfigureIdpRequest struct {
	IdpAlias         string
	Protocol         domain.IdpProtocol
	DisplayName      string
	Config           map[string]any // non-secret protocol params
	Secret           string
	Acknowledgements []string // check IDs the admin has acknowledged
}

// ConfigureIdpResult is the RP-6 response body (§5.2).
type ConfigureIdpResult struct {
	IdpAlias string `json:"idp_alias"`
	Status   string `json:"status"`
}

// ConfigureIdp implements RP-6 (§8.4-8.5): configures a SAML/OIDC IdP on
// the dedicated realm, behind the §11.6 gate. CRITICAL failures block with
// 400; unacknowledged WARNs return 422; pass/acked writes the secret to
// OpenBao then creates/updates the Keycloak IdP.
func (s *IdpService) ConfigureIdp(ctx context.Context, tenantID uuid.UUID, req ConfigureIdpRequest) (*ConfigureIdpResult, error) {
	realm, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if !realm.IsDedicated() {
		return nil, domain.NewError(domain.ErrIdpRequiresDedicatedRealm, "IdP configuration requires a dedicated realm")
	}

	acked := map[string]bool{}
	for _, a := range req.Acknowledgements {
		acked[a] = true
	}
	validation := ValidateIdpConfig(req.Protocol, req.Config, acked)
	metrics.IdpValidationTotal.WithLabelValues(string(validation.Outcome)).Inc()

	checksJSON := map[string]string{}
	for id, outcome := range validation.Checks {
		checksJSON[id] = string(outcome)
	}
	var acks []domain.IdpValidationAck
	now := time.Now().UTC()
	for _, id := range req.Acknowledgements {
		acks = append(acks, domain.IdpValidationAck{CheckID: id, At: now})
	}
	if err := s.validations.Insert(ctx, &domain.IdpConfigValidation{
		TenantID: tenantID, IdpAlias: req.IdpAlias, ValidatorVersion: ValidatorVersion,
		Outcome: validation.Outcome, Checks: checksJSON, Acknowledgements: acks,
	}); err != nil {
		s.log.WarnContext(ctx, "configure_idp: record validation failed", "tenant_id", tenantID, "error", err.Error())
	}

	if validation.Outcome == domain.ValidationOutcomeFailing {
		return nil, domain.NewError(domain.ErrIdpRegionPortabilityFailed, "one or more §11.6 CRITICAL checks failed").
			WithDetails(map[string]any{"validator_version": ValidatorVersion, "failed_checks": validation.Failed, "warnings": validation.Warned})
	}
	if validation.Outcome == domain.ValidationOutcomeWarningsOnly {
		return nil, domain.NewError(domain.ErrIdpWarningsUnacknowledged, "one or more §11.6 WARN checks require acknowledgement").
			WithDetails(map[string]any{"warnings": validation.Warned})
	}

	if req.Secret != "" {
		if err := s.openbao.WriteIdpSecret(ctx, tenantID, req.IdpAlias, map[string]any{"secret": req.Secret}); err != nil {
			return nil, err
		}
	}
	if err := s.keycloak.CreateOrUpdateIdp(ctx, realm.RealmName, port.IdpKeycloakConfig{
		Alias: req.IdpAlias, Protocol: string(req.Protocol), DisplayName: req.DisplayName,
		Enabled: true, Config: req.Config, Secret: req.Secret,
	}); err != nil {
		return nil, err
	}

	if err := s.idpConfigs.Upsert(ctx, &domain.IdpConfig{
		TenantID: tenantID, IdpAlias: req.IdpAlias, Protocol: req.Protocol,
		DisplayName: req.DisplayName, Enabled: true, Config: req.Config,
		Status: domain.IdpConfigStatusActive,
	}); err != nil {
		return nil, err
	}

	return &ConfigureIdpResult{IdpAlias: req.IdpAlias, Status: string(domain.IdpConfigStatusActive)}, nil
}

// RemoveIdp implements RP-7: removes the Keycloak IdP, the OpenBao secret,
// and the local row. Idempotent.
func (s *IdpService) RemoveIdp(ctx context.Context, tenantID uuid.UUID, idpAlias string) error {
	realm, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := s.keycloak.DeleteIdp(ctx, realm.RealmName, idpAlias); err != nil {
		return err
	}
	if err := s.openbao.DeleteIdpSecret(ctx, tenantID, idpAlias); err != nil {
		return err
	}
	return s.idpConfigs.Delete(ctx, tenantID, idpAlias)
}

// PortabilityAuditResult is the RP-13 response body (HLD §11.6 verbatim shape).
type PortabilityAuditResult struct {
	TotalTenants    int      `json:"total_tenants"`
	TotalIdpConfigs int      `json:"total_idp_configs"`
	Passing         int      `json:"passing"`
	WarningsOnly    int      `json:"warnings_only"`
	Failing         int      `json:"failing"`
	FailingTenants  []string `json:"failing_tenants"`
}

// PortabilityAudit implements RP-13: operator-only, runs the §11.6
// checklist against every tenant's live IdP configs by re-validating each
// stored config (its non-secret Config — the audit does not re-read
// secrets from OpenBao).
func (s *IdpService) PortabilityAudit(ctx context.Context) (*PortabilityAuditResult, error) {
	configs, err := s.idpConfigs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	result := &PortabilityAuditResult{TotalIdpConfigs: len(configs), FailingTenants: []string{}}
	tenants := map[uuid.UUID]bool{}
	failingTenants := map[uuid.UUID]bool{}
	for _, c := range configs {
		tenants[c.TenantID] = true
		v := ValidateIdpConfig(c.Protocol, c.Config, nil)
		switch v.Outcome {
		case domain.ValidationOutcomePass:
			result.Passing++
		case domain.ValidationOutcomeWarningsOnly:
			result.WarningsOnly++
		case domain.ValidationOutcomeFailing:
			result.Failing++
			failingTenants[c.TenantID] = true
		}
	}
	result.TotalTenants = len(tenants)
	for id := range failingTenants {
		result.FailingTenants = append(result.FailingTenants, id.String())
	}
	return result, nil
}
