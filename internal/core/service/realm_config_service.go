package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// RealmConfigService implements RP-8 (§8.10, RP-INV-6) and the RP-C4
// reconciler's retry effect.
type RealmConfigService struct {
	realms          port.TenantRealmRepository
	reconciliations port.ReconciliationRepository
	keycloak        port.KeycloakAdmin
	log             port.SlogStyleLogger
}

// NewRealmConfigService wires RealmConfigService's dependencies.
func NewRealmConfigService(realms port.TenantRealmRepository, reconciliations port.ReconciliationRepository, keycloak port.KeycloakAdmin) *RealmConfigService {
	return &RealmConfigService{realms: realms, reconciliations: reconciliations, keycloak: keycloak}
}

// WithLogger attaches a logger and returns s for chaining.
func (s *RealmConfigService) WithLogger(log port.Logger) *RealmConfigService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// ApplyRealmConfigResult is the RP-8 response body (§8.10).
type ApplyRealmConfigResult struct {
	Applied     bool `json:"applied"`
	Reconciling bool `json:"reconciling,omitempty"`
}

// ApplyRealmConfig implements RP-8 (RP-INV-6): applies a realm-affecting
// setting synchronously, never over the bus. On a transient Keycloak
// failure, records the desired value as `pending` for RP-C4 to retry
// instead of failing the caller outright (local-intent-first, §9.1).
func (s *RealmConfigService) ApplyRealmConfig(ctx context.Context, tenantID uuid.UUID, settingKey string, desiredValue any) (*ApplyRealmConfigResult, error) {
	if !domain.IsAllowedSettingKey(settingKey) {
		return nil, domain.NewError(domain.ErrUnsupportedRealmSetting, "setting_key is not on the realm-config allow-list").
			WithDetails(map[string]any{"allowed": []string{"local_accounts_enabled", "idp:<alias>"}})
	}
	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	if err := s.keycloak.ApplyRealmConfig(ctx, row.RealmName, settingKey, desiredValue); err != nil {
		raw := marshalIdempotencyResult(ctx, s.log, "apply_realm_config", desiredValue)
		if recErr := s.reconciliations.Insert(ctx, &domain.RealmConfigReconciliation{
			TenantID: tenantID, SettingKey: settingKey, DesiredValue: raw,
		}); recErr != nil {
			s.log.WarnContext(ctx, "apply_realm_config: record pending reconciliation failed", "tenant_id", tenantID, "error", recErr.Error())
		}
		return &ApplyRealmConfigResult{Applied: false, Reconciling: true}, nil
	}
	return &ApplyRealmConfigResult{Applied: true}, nil
}
