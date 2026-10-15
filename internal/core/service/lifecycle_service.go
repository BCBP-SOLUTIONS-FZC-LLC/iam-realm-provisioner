package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// LifecycleService implements RP-4, RP-10, RP-11, RP-12, RP-14, RP-15,
// RP-16, and the RP-C1/RP-C3 reconciler effects (§8.4, §8.7-8.9, §8.11-8.13).
type LifecycleService struct {
	realms      port.TenantRealmRepository
	idpCfg      port.IdpConfigRepository
	idempotency port.IdempotencyStore
	keycloak    port.KeycloakAdmin
	tx          port.TxRunner
	log         port.SlogStyleLogger
	sharedRealm string
	revertDays  int
}

// NewLifecycleService wires LifecycleService's dependencies.
func NewLifecycleService(
	realms port.TenantRealmRepository,
	idempotency port.IdempotencyStore,
	keycloak port.KeycloakAdmin,
	tx port.TxRunner,
	sharedRealm string,
) *LifecycleService {
	if sharedRealm == "" {
		sharedRealm = "trial"
	}
	return &LifecycleService{realms: realms, idempotency: idempotency, keycloak: keycloak, tx: tx, sharedRealm: sharedRealm, revertDays: 7}
}

// WithIdpConfigs attaches the IdP-config repository so RP-11 can include
// idp_count in its response (CD-20: interim idp_count guard for plan-change-check).
func (s *LifecycleService) WithIdpConfigs(idpCfg port.IdpConfigRepository) *LifecycleService {
	s.idpCfg = idpCfg
	return s
}

// WithLogger attaches a logger and returns s for chaining.
func (s *LifecycleService) WithLogger(log port.Logger) *LifecycleService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// failIdempotency marks idempotencyKey's provisioning_operations row
// failed. Best-effort: the original error is what the caller returns
// regardless, so a failure here is only logged.
func (s *LifecycleService) failIdempotency(ctx context.Context, idempotencyKey string) {
	if err := s.idempotency.Fail(ctx, idempotencyKey); err != nil {
		s.log.WarnContext(ctx, "idempotency: mark failed did not persist", "idempotency_key", idempotencyKey, "error", err.Error())
	}
}

// completeIdempotency marshals result and marks idempotencyKey succeeded,
// for the idempotent-no-op short-circuit paths below. Both failure modes
// are logged, not propagated, since the underlying operation already
// succeeded (it was already in the desired state).
func (s *LifecycleService) completeIdempotency(ctx context.Context, idempotencyKey, op string, result any) {
	body := marshalIdempotencyResult(ctx, s.log, op, result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, op+": idempotency complete failed", "idempotency_key", idempotencyKey, "error", err.Error())
	}
}

// ── RP-11: GET realm (read-only) ─────────────────────────────────────────

// GetRealmResult is the RP-11 response body (§5.1, CD-20).
// idp_count is the number of active (status='active') SAML/OIDC IdP
// configurations for the tenant. Callers: Core (I-18 plan-change-check
// SSO-downgrade guard, CD-20), Billing (interim guard, CD-20; network
// CD-19 NetworkPolicy + mesh AuthorizationPolicy implemented in deploy/helm/templates/),
// Signup BFF, Org & Membership (both iam-system).
//
// A non-zero idp_count signals active federated SSO. Core uses it to block a
// plan downgrade that would revoke SSO access for active federated users.
type GetRealmResult struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	RealmName     string    `json:"realm_name"`
	Strategy      string    `json:"strategy"`
	KeycloakShard *string   `json:"keycloak_shard,omitempty"`
	Status        string    `json:"status"`
	RecordVersion int64     `json:"record_version"`
	// IdpCount is the number of active SAML/OIDC IdP configurations for the
	// tenant (status='active' only; disabled and pending_validation excluded).
	// Always 0 for shared-strategy (trial) tenants — RP-6 blocks IdP
	// configuration on the shared realm (409 idp_requires_dedicated_realm).
	IdpCount int `json:"idp_count"`
}

// GetRealm implements RP-11: returns tenantID's current realm state including
// the count of active IdP configurations (CD-20 SSO-downgrade guard).
//
// Error behavior:
//   - Unknown tenant                   → 404 tenant_realm_not_found
//   - Trial (shared) tenant            → 200 idp_count=0 (no DB query — trial
//     tenants cannot configure SSO; RP-6 returns 409 idp_requires_dedicated_realm)
//   - IdP count query fails            → propagate; DB outage → 503, timeout → 503
//   - idpCfg not wired (misconfigured) → 500 internal_error
func (s *LifecycleService) GetRealm(ctx context.Context, tenantID uuid.UUID) (*GetRealmResult, error) {
	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	res := &GetRealmResult{
		TenantID: row.TenantID, RealmName: row.RealmName, Strategy: string(row.Strategy),
		KeycloakShard: row.KeycloakShard, Status: string(row.Status), RecordVersion: row.RecordVersion,
	}
	// Trial tenants on the shared realm cannot configure SSO (RP-6 returns
	// 409 idp_requires_dedicated_realm), so idp_count is definitionally 0.
	// Skip the DB round-trip and return immediately.
	if row.Strategy == domain.RealmStrategyShared {
		return res, nil
	}
	if s.idpCfg == nil {
		return nil, domain.NewError(domain.ErrDBUnavailable,
			"idp_count repository not wired — WithIdpConfigs required for dedicated-realm tenants")
	}
	n, err := s.idpCfg.CountActiveByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	res.IdpCount = n
	return res, nil
}

// ── RP-10: reactivate ─────────────────────────────────────────────────────

// ReactivateResult is the RP-10 response body.
type ReactivateResult struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Status   string    `json:"status"`
}

// Reactivate implements RP-10 (§8.11): re-enables a disabled trial user or
// a suspended dedicated realm (billing-lapse or operator-sourced — both
// are reversible via this one endpoint). Rejected if offboarded
// (RP-INV-5, terminal). source/reason drive which event is emitted.
func (s *LifecycleService) Reactivate(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, source domain.ReactivationSource, reason string, newTrialEndsAt *time.Time) (*ReactivateResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpReactivate)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ReactivateResult](existing)
	}

	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	if row.IsOffboarded() {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant is offboarded — this transition is terminal (RP-INV-5)")
	}
	if row.Status == domain.RealmStatusProvisioning {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant is still provisioning — cannot reactivate what was never active")
	}
	if row.Status == domain.RealmStatusActive {
		// Idempotent no-op — already active.
		result := &ReactivateResult{TenantID: tenantID, Status: string(row.Status)}
		s.completeIdempotency(ctx, idempotencyKey, "reactivate", result)
		return result, nil
	}

	if row.Strategy == domain.RealmStrategyShared {
		if err := s.keycloak.EnableUsersInRealm(ctx, row.RealmName, tenantID); err != nil {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, err
		}
	} else {
		if err := s.keycloak.EnableRealm(ctx, row.RealmName); err != nil {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, err
		}
	}

	result := &ReactivateResult{TenantID: tenantID, Status: string(domain.RealmStatusActive)}
	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		row.Status = domain.RealmStatusActive
		row.DisabledAt = nil
		row.SuspensionSource = nil
		if row.Strategy == domain.RealmStrategyShared {
			if newTrialEndsAt == nil {
				extended := time.Now().UTC().AddDate(0, 0, 30)
				newTrialEndsAt = &extended
			}
			row.TrialEndsAt = newTrialEndsAt
		}
		if err := s.realms.Update(ctx, row); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		if row.Strategy == domain.RealmStrategyShared {
			ends := newTrialEndsAt.UTC().Format(time.RFC3339)
			return pub.Enqueue(ctx, &domain.Event{
				Type: domain.EventTrialReactivated, TenantID: tenantID,
				Data: domain.TrialReactivatedPayload{TenantID: tenantID, NewTrialEndsAt: ends},
			})
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventTenantReactivated, TenantID: tenantID,
			Data: domain.TenantReactivatedPayload{TenantID: tenantID, Realm: row.RealmName, Source: source, Reason: reason},
		})
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	body := marshalIdempotencyResult(ctx, s.log, "reactivate", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "reactivate: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// ── RP-12: expire-trial (reconciler-triggered, per-tenant, §8.7) ────────

// ExpireTrialResult is the RP-12 response body.
type ExpireTrialResult struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	TrialEndsAt string    `json:"trial_ends_at"`
	GraceEndsAt string    `json:"grace_ends_at"`
}

// ExpireTrial implements RP-12 (§8.7): idempotent per-tenant trial-expiry
// effect — mark disabled_at, disable the tenant's shared-realm users, emit
// TrialExpired. Called once per expired row by the RP-C1 cron.
func (s *LifecycleService) ExpireTrial(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, graceDays int) (*ExpireTrialResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpExpireTrial)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ExpireTrialResult](existing)
	}

	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	if row.Strategy != domain.RealmStrategyShared {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrRealmAlreadyDedicated, "expire-trial only applies to shared-strategy tenants")
	}
	if row.IsOffboarded() {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant has been offboarded")
	}
	if row.Status == domain.RealmStatusSuspended {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant is suspended")
	}

	if err := s.keycloak.DisableUsersInRealm(ctx, row.RealmName, tenantID); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	now := time.Now().UTC()
	graceEnds := now.AddDate(0, 0, graceDays)
	trialEndsAt := now
	if row.TrialEndsAt != nil {
		trialEndsAt = *row.TrialEndsAt
	}
	result := &ExpireTrialResult{TenantID: tenantID, TrialEndsAt: trialEndsAt.Format(time.RFC3339), GraceEndsAt: graceEnds.Format(time.RFC3339)}

	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		row.Status = domain.RealmStatusDisabled
		row.DisabledAt = &now
		if err := s.realms.Update(ctx, row); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventTrialExpired, TenantID: tenantID, Actor: domain.SystemActorID,
			Data: domain.TrialExpiredPayload{TenantID: tenantID, TrialEndsAt: result.TrialEndsAt, GraceEndsAt: result.GraceEndsAt},
		})
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	body := marshalIdempotencyResult(ctx, s.log, "expire_trial", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "expire_trial: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// ── RP-14: emergency operator suspension (§8.12) ─────────────────────────

// SuspendResult is the RP-14 response body.
type SuspendResult struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Status   string    `json:"status"`
}

// Suspend implements RP-14 (§8.12, RP-D12): operator-only immediate
// lockout. Effect-only — never sets subscription_status (O&M's job).
// Reversible via Reactivate.
func (s *LifecycleService) Suspend(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, reason string) (*SuspendResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpSuspend)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[SuspendResult](existing)
	}

	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	if row.IsOffboarded() {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant is offboarded")
	}
	if row.Status == domain.RealmStatusSuspended {
		result := &SuspendResult{TenantID: tenantID, Status: string(row.Status)}
		s.completeIdempotency(ctx, idempotencyKey, "suspend", result)
		return result, nil
	}

	if row.Strategy == domain.RealmStrategyDedicated {
		if err := s.keycloak.DisableRealm(ctx, row.RealmName); err != nil {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, err
		}
	} else {
		// Shared-trial tenant: disable ONLY that tenant's users, NEVER the
		// shared realm itself (RP-INV-2 — it hosts every trial tenant).
		if err := s.keycloak.DisableUsersInRealm(ctx, row.RealmName, tenantID); err != nil {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, err
		}
	}

	result := &SuspendResult{TenantID: tenantID, Status: string(domain.RealmStatusSuspended)}
	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		now := time.Now().UTC()
		row.Status = domain.RealmStatusSuspended
		row.DisabledAt = &now
		source := domain.SuspensionSourceOperator
		row.SuspensionSource = &source
		if err := s.realms.Update(ctx, row); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventTenantSuspended, TenantID: tenantID,
			Data: domain.TenantSuspendedPayload{
				TenantID: tenantID, Realm: row.RealmName, SuspendedAt: now.Format(time.RFC3339),
				Source: domain.SuspensionSourceOperator, Reason: reason,
			},
		})
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	body := marshalIdempotencyResult(ctx, s.log, "suspend", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "suspend: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// ── RP-15: delete user (idempotent, §8.13) ───────────────────────────────

// DeleteUser implements RP-15: idempotent Keycloak user delete — an absent
// user is a success (§9, O&M's PI-9 reconciler retries safely).
func (s *LifecycleService) DeleteUser(ctx context.Context, tenantID, keycloakUserID uuid.UUID) (err error) {
	defer func() { metrics.UserDeleteTotal.WithLabelValues(outcome(err)).Inc() }()

	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.keycloak.DeleteUser(ctx, row.RealmName, keycloakUserID)
}

// ── RP-16: revoke sessions (idempotent, best-effort/fail-open on O&M's
// side, §8.13) ────────────────────────────────────────────────────────────

// RevokeSessions implements RP-16: idempotent session/token revocation.
func (s *LifecycleService) RevokeSessions(ctx context.Context, tenantID, keycloakUserID uuid.UUID) (err error) {
	defer func() { metrics.SessionRevokeTotal.WithLabelValues(outcome(err)).Inc() }()

	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.keycloak.RevokeUserSessions(ctx, row.RealmName, keycloakUserID)
}
