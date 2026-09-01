// Package service implements the business logic behind the RP-1..RP-16
// endpoints and the reconciler jobs — domain + port only, no adapter/Gin/
// pgx/gocloak/AWS import (§3.2/§3.4).
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// nowPtr returns a pointer to the current UTC time — a small helper since
// domain.TenantRealm's timestamp fields are *time.Time.
func nowPtr() *time.Time {
	t := time.Now().UTC()
	return &t
}

// ProvisioningService implements RP-1/RP-2/RP-3/RP-4 (§8.1-8.4).
type ProvisioningService struct {
	realms      port.TenantRealmRepository
	idempotency port.IdempotencyStore
	keycloak    port.KeycloakAdmin
	tx          port.TxRunner
	log         port.SlogStyleLogger
	sharedRealm string
	shardIDs    []string
}

// NewProvisioningService wires ProvisioningService's dependencies.
func NewProvisioningService(
	realms port.TenantRealmRepository,
	idempotency port.IdempotencyStore,
	keycloak port.KeycloakAdmin,
	tx port.TxRunner,
	sharedRealm string,
	shardIDs []string,
) *ProvisioningService {
	if sharedRealm == "" {
		sharedRealm = "trial"
	}
	if len(shardIDs) == 0 {
		shardIDs = []string{"kc-shard-1"}
	}
	return &ProvisioningService{realms: realms, idempotency: idempotency, keycloak: keycloak, tx: tx, sharedRealm: sharedRealm, shardIDs: shardIDs}
}

// WithLogger attaches a logger and returns s for chaining.
func (s *ProvisioningService) WithLogger(log port.Logger) *ProvisioningService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// failIdempotency marks idempotencyKey's provisioning_operations row
// failed. This is a best-effort side effect on an already-failing path —
// the original error is what the caller returns regardless of whether this
// succeeds, so a failure here is only logged.
func (s *ProvisioningService) failIdempotency(ctx context.Context, idempotencyKey string) {
	if err := s.idempotency.Fail(ctx, idempotencyKey); err != nil {
		s.log.WarnContext(ctx, "idempotency: mark failed did not persist", "idempotency_key", idempotencyKey, "error", err.Error())
	}
}

// bestEffortDeleteRealm deletes realmName as compensating teardown on a
// provisioning failure (§9.3). Best-effort: the caller already has the
// primary error to return, so a teardown failure here is only logged (an
// orphaned realm is caught by the RP-C reconcilers / manual cleanup).
func (s *ProvisioningService) bestEffortDeleteRealm(ctx context.Context, realmName string) {
	if err := s.keycloak.DeleteRealmIfExists(ctx, realmName); err != nil {
		s.log.WarnContext(ctx, "compensating teardown: delete realm failed", "realm", realmName, "error", err.Error())
	}
}

// marshalIdempotencyResult marshals v for storage in the idempotency
// ledger. A marshal failure is logged, not propagated — the caller stores
// whatever bytes result (matching the pre-existing best-effort semantics)
// since the operation itself already succeeded.
func marshalIdempotencyResult(ctx context.Context, log port.SlogStyleLogger, op string, v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		log.WarnContext(ctx, op+": marshal idempotency result failed", "error", err.Error())
	}
	return body
}

// ProvisionTrialRequest is the RP-1 request body (§5.2).
type ProvisionTrialRequest struct {
	TenantID           uuid.UUID
	TenantName         string
	TenantSlug         string
	AdminEmail         string
	AdminName          string
	HashedPasswordSeed string
	Plan               string
}

// ProvisionTrialResult is the RP-1 response body (§5.2).
type ProvisionTrialResult struct {
	TenantID              uuid.UUID `json:"tenant_id"`
	Realm                 string    `json:"realm"`
	Strategy              string    `json:"strategy"`
	KeycloakUserID        uuid.UUID `json:"keycloak_user_id"`
	EmailVerificationSent bool      `json:"email_verification_sent"`
}

// ProvisionTrial implements RP-1 (§8.1): create the trial user in the
// shared realm, stamp tenant_id, trigger verification email, and emit
// TrialTenantProvisioned. Keycloak-effect-first, then local commit (§9.1).
// Idempotent via idempotencyKey (§9.2).
func (s *ProvisioningService) ProvisionTrial(ctx context.Context, idempotencyKey string, req ProvisionTrialRequest) (*ProvisionTrialResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, req.TenantID, domain.OpProvisionTrial)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ProvisionTrialResult](existing)
	}

	kcUserID, err := s.keycloak.CreateTrialUser(ctx, port.CreateUserRequest{
		Email: req.AdminEmail, FullName: req.AdminName, TenantID: req.TenantID,
		HashedPasswordSeed: req.HashedPasswordSeed, EmailVerified: false,
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	result := &ProvisionTrialResult{
		TenantID: req.TenantID, Realm: s.sharedRealm, Strategy: string(domain.RealmStrategyShared),
		KeycloakUserID: kcUserID, EmailVerificationSent: true,
	}

	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		if err := s.realms.Upsert(ctx, &domain.TenantRealm{
			TenantID: req.TenantID, RealmName: s.sharedRealm, Strategy: domain.RealmStrategyShared,
			Status: domain.RealmStatusActive,
		}); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.EnqueueCtx(ctx, &domain.Event{
			Type: domain.EventTrialTenantProvisioned, TenantID: req.TenantID,
			Subject: "tenants/" + req.TenantID.String(),
			Data: domain.TrialTenantProvisionedPayload{
				TenantID: req.TenantID, Realm: s.sharedRealm, KeycloakUserID: kcUserID,
			},
		})
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	body := marshalIdempotencyResult(ctx, s.log, "provision_trial", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "provision_trial: idempotency complete failed", "tenant_id", req.TenantID, "error", err.Error())
	}
	return result, nil
}

// selectShard places a new dedicated realm on the least-loaded shard,
// reading current per-shard counts from the local mirror (§9.5). Ties
// break deterministically by shard id (lexicographic) so two concurrent
// placements don't thrash.
func (s *ProvisioningService) selectShard(ctx context.Context) (string, error) {
	counts, err := s.realms.CountsByShard(ctx)
	if err != nil {
		return "", err
	}
	best := s.shardIDs[0]
	bestCount := counts[best]
	for _, id := range s.shardIDs[1:] {
		if c := counts[id]; c < bestCount || (c == bestCount && id < best) {
			best, bestCount = id, c
		}
	}
	return best, nil
}

// dedicatedRealmName derives the dedicated realm name from a tenant slug
// (§4.2: "tenant-<slug>").
func dedicatedRealmName(slug string) string { return "tenant-" + slug }

// ProvisionDirectRequest is the RP-2 request body (§5.2, parity with RP-1).
type ProvisionDirectRequest struct {
	TenantID              uuid.UUID
	TenantName            string
	TenantSlug            string
	AdminEmail            string
	AdminName             string
	HashedPasswordSeed    string
	Plan                  string
	BillingSubscriptionID string
}

// ProvisionDirectResult is the RP-2 response body (§5.2).
type ProvisionDirectResult struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	Realm         string    `json:"realm"`
	Strategy      string    `json:"strategy"`
	KeycloakShard string    `json:"keycloak_shard"`
	OIDCClients   []string  `json:"oidc_clients"`
}

// ProvisionDirect implements RP-2 (§8.2): provision the dedicated realm
// straight away (no shared-realm intermediate), create the admin user, and
// emit TenantRealmReady + DirectPaidSignup + TenantConverted(direct_signup=true).
func (s *ProvisioningService) ProvisionDirect(ctx context.Context, idempotencyKey string, req ProvisionDirectRequest) (*ProvisionDirectResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, req.TenantID, domain.OpProvisionDirect)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ProvisionDirectResult](existing)
	}

	realmName := dedicatedRealmName(req.TenantSlug)
	shard, err := s.selectShard(ctx)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	if err := s.keycloak.CreateRealmFromTemplate(ctx, realmName, "v1"); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	clients, err := s.keycloak.RegisterOIDCClients(ctx, realmName, req.TenantSlug)
	if err != nil {
		s.bestEffortDeleteRealm(ctx, realmName)
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	if _, err := s.keycloak.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: req.AdminEmail, FullName: req.AdminName, TenantID: req.TenantID,
		HashedPasswordSeed: req.HashedPasswordSeed, EmailVerified: false,
		RequiredActions: []string{"VERIFY_EMAIL"},
	}); err != nil {
		s.bestEffortDeleteRealm(ctx, realmName)
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	result := &ProvisionDirectResult{
		TenantID: req.TenantID, Realm: realmName, Strategy: string(domain.RealmStrategyDedicated),
		KeycloakShard: shard, OIDCClients: clients,
	}

	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		if err := s.realms.Upsert(ctx, &domain.TenantRealm{
			TenantID: req.TenantID, RealmName: realmName, Strategy: domain.RealmStrategyDedicated,
			KeycloakShard: &shard, Status: domain.RealmStatusActive, ConvertedAt: nowPtr(),
		}); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		if err := pub.EnqueueCtx(ctx, &domain.Event{
			Type: domain.EventTenantRealmReady, TenantID: req.TenantID,
			Subject: "tenants/" + req.TenantID.String(),
			Data: domain.TenantRealmReadyPayload{
				TenantID: req.TenantID, Realm: realmName, KeycloakShard: shard, OIDCClients: clients,
			},
		}); err != nil {
			return err
		}
		if err := pub.EnqueueCtx(ctx, &domain.Event{
			Type: domain.EventDirectPaidSignup, TenantID: req.TenantID,
			Subject: "tenants/" + req.TenantID.String(),
			Data: domain.DirectPaidSignupPayload{
				TenantID: req.TenantID, Realm: realmName, DirectSignup: true,
			},
		}); err != nil {
			return err
		}
		var billingSubID *string
		if req.BillingSubscriptionID != "" {
			billingSubID = &req.BillingSubscriptionID
		}
		return pub.EnqueueCtx(ctx, &domain.Event{
			Type: domain.EventTenantConverted, TenantID: req.TenantID,
			Subject: "tenants/" + req.TenantID.String(),
			Data: domain.TenantConvertedPayload{
				TenantID: req.TenantID, Realm: realmName, Plan: req.Plan,
				BillingSubscriptionID: billingSubID, UsersMigrated: 0, DirectSignup: true,
			},
		})
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	body := marshalIdempotencyResult(ctx, s.log, "provision_direct", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "provision_direct: idempotency complete failed", "tenant_id", req.TenantID, "error", err.Error())
	}
	return result, nil
}

// ConvertRequest is the RP-3 request body (§5.2). PaymentMethodID is
// accepted but ignored/never stored — Billing/PCI domain (§18.1).
type ConvertRequest struct {
	Plan                  string
	BillingSubscriptionID string
	PaymentMethodID       string // accepted but ignored/never stored, §5.2.1
}

// ConvertResult is the RP-3 response body (§5.2).
type ConvertResult struct {
	TenantID                 uuid.UUID `json:"tenant_id"`
	Realm                    string    `json:"realm"`
	Strategy                 string    `json:"strategy"`
	KeycloakShard            string    `json:"keycloak_shard"`
	OIDCClients              []string  `json:"oidc_clients"`
	UsersMigrated            int       `json:"users_migrated"`
	WebauthnReenrollRequired int       `json:"webauthn_reenroll_required"`
	SharedSessionsTerminated bool      `json:"shared_sessions_terminated"`
}

// Convert implements RP-3 (§8.3): trial->paid conversion with user
// migration. Requires the tenant to currently be `shared` strategy.
func (s *ProvisioningService) Convert(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, req ConvertRequest) (*ConvertResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpConvert)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ConvertResult](existing)
	}

	current, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	if current.Strategy != domain.RealmStrategyShared {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrRealmAlreadyDedicated, "tenant is already dedicated").WithDetails(map[string]any{"tenant_id": tenantID})
	}

	// derive a slug from the tenant id when none is tracked locally (this
	// service persists no tenant_slug — see §5.2.1 field disposition); a
	// caller-visible slug MUST be supplied via the realm-name convention
	// upstream. Convert's request body carries no slug per §5.2, so this
	// derives a stable, collision-resistant name from the tenant id.
	slug := tenantID.String()[:8]
	realmName := dedicatedRealmName(slug)
	shard, err := s.selectShard(ctx)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	if err := s.keycloak.CreateRealmFromTemplate(ctx, realmName, "v1"); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	clients, err := s.keycloak.RegisterOIDCClients(ctx, realmName, slug)
	if err != nil {
		s.bestEffortDeleteRealm(ctx, realmName)
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrConversionFailed, "register oidc clients failed").WithDetails(map[string]any{"error": err.Error()})
	}

	sourceUsers, err := s.keycloak.ListUsersByTenant(ctx, current.RealmName, tenantID)
	if err != nil {
		s.bestEffortDeleteRealm(ctx, realmName)
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrConversionFailed, "list trial users failed").WithDetails(map[string]any{"error": err.Error()})
	}

	webauthnReenroll := 0
	for _, uid := range sourceUsers {
		reenroll, err := s.keycloak.MigrateUser(ctx, current.RealmName, realmName, uid)
		if err != nil {
			// Compensating teardown (§9.3): the new dedicated realm is
			// deleted — the shared realm is untouched (users already
			// migrated in this loop were deleted from the source as part
			// of MigrateUser's own semantics; a retry with the same
			// Idempotency-Key re-lists whatever remains in the shared
			// realm and continues, since this is a fresh FindByTenantID
			// + ListUsersByTenant on every attempt).
			s.bestEffortDeleteRealm(ctx, realmName)
			s.failIdempotency(ctx, idempotencyKey)
			return nil, domain.NewError(domain.ErrConversionFailed, "migrate user failed").WithDetails(map[string]any{"error": err.Error(), "keycloak_user_id": uid})
		}
		if reenroll {
			webauthnReenroll++
		}
	}

	if err := s.keycloak.ForceLogoutRealm(ctx, current.RealmName); err != nil {
		s.log.WarnContext(ctx, "convert: force_logout_realm failed (non-fatal, sessions expire naturally)", "tenant_id", tenantID, "error", err.Error())
	}

	result := &ConvertResult{
		TenantID: tenantID, Realm: realmName, Strategy: string(domain.RealmStrategyDedicated),
		KeycloakShard: shard, OIDCClients: clients, UsersMigrated: len(sourceUsers),
		WebauthnReenrollRequired: webauthnReenroll, SharedSessionsTerminated: true,
	}

	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		current.Strategy = domain.RealmStrategyDedicated
		current.RealmName = realmName
		current.KeycloakShard = &shard
		current.Status = domain.RealmStatusActive
		current.ConvertedAt = nowPtr()
		if err := s.realms.Update(ctx, current); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		if err := pub.EnqueueCtx(ctx, &domain.Event{
			Type: domain.EventTenantRealmReady, TenantID: tenantID,
			Subject: "tenants/" + tenantID.String(),
			Data: domain.TenantRealmReadyPayload{
				TenantID: tenantID, Realm: realmName, KeycloakShard: shard, OIDCClients: clients,
			},
		}); err != nil {
			return err
		}
		var billingSubID *string
		if req.BillingSubscriptionID != "" {
			billingSubID = &req.BillingSubscriptionID
		}
		return pub.EnqueueCtx(ctx, &domain.Event{
			Type: domain.EventTenantConverted, TenantID: tenantID,
			Subject: "tenants/" + tenantID.String(),
			Data: domain.TenantConvertedPayload{
				TenantID: tenantID, Realm: realmName, Plan: req.Plan,
				BillingSubscriptionID: billingSubID, UsersMigrated: len(sourceUsers), DirectSignup: false,
			},
		})
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	body := marshalIdempotencyResult(ctx, s.log, "convert", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "convert: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// CreateInvitedUserRequest is the RP-5 request body (§5.2, F5 — RP applies
// O&M's required_actions verbatim, never re-deriving them).
type CreateInvitedUserRequest struct {
	Email           string
	FullName        string
	RequiredActions []string
}

// CreateInvitedUserResult is the RP-5 response body.
type CreateInvitedUserResult struct {
	KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
}

// CreateInvitedUser implements RP-5: creates an invited user in the
// tenant's realm (resolved from tenant_realms). No local persistence — RP
// has no per-user table (§5.2.1).
func (s *ProvisioningService) CreateInvitedUser(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, req CreateInvitedUserRequest) (*CreateInvitedUserResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpCreateInvitedUser)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[CreateInvitedUserResult](existing)
	}

	row, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	userID, err := s.keycloak.CreateInvitedUser(ctx, row.RealmName, port.CreateUserRequest{
		Email: req.Email, FullName: req.FullName, TenantID: tenantID,
		RequiredActions: req.RequiredActions, EmailVerified: false,
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	result := &CreateInvitedUserResult{KeycloakUserID: userID}
	body := marshalIdempotencyResult(ctx, s.log, "create_invited_user", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "create_invited_user: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// RevertConversionResult is the RP-4 response body.
type RevertConversionResult struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Realm    string    `json:"realm"`
	Strategy string    `json:"strategy"`
}

// RevertConversion implements RP-4 (§8.4): the within-7-days undo of
// Convert — recreates the tenant in the shared trial realm (extended trial
// expiry), migrates users back, and tears down the dedicated realm. Beyond
// the window, returns 409 revert_window_expired (late reversion is an
// operator runbook, §22).
func (s *ProvisioningService) RevertConversion(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, _ string, revertWindowDays, newTrialDurationDays int) (*RevertConversionResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpRevertConversion)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[RevertConversionResult](existing)
	}

	current, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	if current.Strategy != domain.RealmStrategyDedicated {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrInvalidRequest, "revert-conversion requires a dedicated-strategy tenant")
	}
	if current.ConvertedAt == nil || time.Since(*current.ConvertedAt) > time.Duration(revertWindowDays)*24*time.Hour {
		convertedAt := ""
		if current.ConvertedAt != nil {
			convertedAt = current.ConvertedAt.UTC().Format(time.RFC3339)
		}
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrRevertWindowExpired, "conversion reversal window has expired").
			WithDetails(map[string]any{"converted_at": convertedAt})
	}

	dedicatedRealm := current.RealmName
	sourceUsers, err := s.keycloak.ListUsersByTenant(ctx, dedicatedRealm, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrConversionFailed, "list dedicated-realm users failed").WithDetails(map[string]any{"error": err.Error()})
	}
	for _, uid := range sourceUsers {
		if _, err := s.keycloak.MigrateUser(ctx, dedicatedRealm, s.sharedRealm, uid); err != nil {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, domain.NewError(domain.ErrConversionFailed, "migrate user back to shared realm failed").WithDetails(map[string]any{"error": err.Error(), "keycloak_user_id": uid})
		}
	}
	if err := s.keycloak.DeleteRealmIfExists(ctx, dedicatedRealm); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	result := &RevertConversionResult{TenantID: tenantID, Realm: s.sharedRealm, Strategy: string(domain.RealmStrategyShared)}
	newTrialEndsAt := time.Now().UTC().AddDate(0, 0, newTrialDurationDays)
	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		current.Strategy = domain.RealmStrategyShared
		current.RealmName = s.sharedRealm
		current.KeycloakShard = nil
		current.ConvertedAt = nil
		current.TrialEndsAt = &newTrialEndsAt
		current.Status = domain.RealmStatusActive
		if err := s.realms.Update(ctx, current); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.EnqueueCtx(ctx, &domain.Event{
			Type: domain.EventTrialReactivated, TenantID: tenantID,
			Data: domain.TrialReactivatedPayload{TenantID: tenantID, NewTrialEndsAt: newTrialEndsAt.Format(time.RFC3339)},
		})
	})
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	body := marshalIdempotencyResult(ctx, s.log, "revert_conversion", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "revert_conversion: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// replayOrConflict inspects an already-began idempotency ledger row: a
// `succeeded` operation replays its stored result; anything else (still
// `in_progress`, or `failed`) surfaces as 409 operation_in_progress so the
// caller retries with the same key (§9.2).
func replayOrConflict[T any](op *domain.ProvisioningOperation) (*T, error) {
	if op.Status == domain.OpStatusSucceeded {
		var out T
		if len(op.Result) > 0 {
			if err := json.Unmarshal(op.Result, &out); err != nil {
				return nil, fmt.Errorf("idempotency: decode stored result: %w", err)
			}
		}
		return &out, nil
	}
	return nil, domain.NewError(domain.ErrOperationInProgress, "a call with this Idempotency-Key is already in progress or previously failed — retry")
}
