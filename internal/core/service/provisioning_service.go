// Package service implements the business logic behind the RP-1..RP-17
// endpoints and the reconciler jobs — domain + port only, no adapter/Gin/
// pgx/gocloak/AWS import (§3.2/§3.4).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// callerActor returns the caller's UUID string for use in event and audit
// records. iam-system (uuid.Nil) maps to SystemActorID; any other non-nil
// UUID (e.g. commercial-system → CommercialActorID) is returned as-is.
func callerActor(ctx context.Context) string {
	if rc, ok := requestctx.FromContext(ctx); ok && rc.UserID != uuid.Nil {
		return rc.UserID.String()
	}
	return domain.SystemActorID
}

// nowPtr returns a pointer to the current UTC time — a small helper since
// domain.TenantRealm's timestamp fields are *time.Time.
func nowPtr() *time.Time {
	t := time.Now().UTC()
	return &t
}

// outcome maps an operation's error to the frozen `result` label value used
// by every result-labeled instrument in §11.2.
func outcome(err error) string {
	if err != nil {
		return "failure"
	}
	return "success"
}

// ProvisioningService implements RP-1/RP-2/RP-3/RP-4 (§8.1-8.4).
type ProvisioningService struct {
	realms       port.TenantRealmRepository
	idempotency  port.IdempotencyStore
	keycloak     port.KeycloakAdmin
	tokenService port.TokenServiceClient
	tx           port.TxRunner
	auditEntries port.PendingAuditEntryRepository
	log          port.SlogStyleLogger
	sharedRealm  string
	shardIDs     []string

	// convertTimeout bounds one detached RP-3 conversion (Convert).
	convertTimeout time.Duration
	// inflight tracks detached conversions so shutdown can wait for them
	// (Drain) before the pools close.
	inflight sync.WaitGroup
}

// DefaultConvertTimeout bounds a detached RP-3 conversion. The LLD budget is
// p95 ≤ 8s + ~0.3s per migrated user (§11.1), so 10 minutes covers ~2,000
// trial users. It must stay below the idempotency ledger's in_progress
// lease (postgres.DefaultInProgressLease, 15m), or a still-running
// conversion could be reclaimed and run twice.
const DefaultConvertTimeout = 10 * time.Minute

// NewProvisioningService wires ProvisioningService's dependencies.
// auditEntries receives RP-1/RP-2's TermsAccepted entry, in the same
// transaction as tenant_realms (§10.6).
func NewProvisioningService(
	realms port.TenantRealmRepository,
	idempotency port.IdempotencyStore,
	keycloak port.KeycloakAdmin,
	tokenService port.TokenServiceClient,
	tx port.TxRunner,
	auditEntries port.PendingAuditEntryRepository,
	sharedRealm string,
	shardIDs []string,
) *ProvisioningService {
	if sharedRealm == "" {
		sharedRealm = "trial"
	}
	if len(shardIDs) == 0 {
		shardIDs = []string{"kc-shard-1"}
	}
	return &ProvisioningService{
		realms: realms, idempotency: idempotency, keycloak: keycloak, tokenService: tokenService,
		tx: tx, auditEntries: auditEntries, sharedRealm: sharedRealm, shardIDs: shardIDs, convertTimeout: DefaultConvertTimeout,
	}
}

// WithConvertTimeout overrides the detached RP-3 conversion budget
// (CONVERT_TIMEOUT) and returns s. Non-positive values are ignored.
func (s *ProvisioningService) WithConvertTimeout(d time.Duration) *ProvisioningService {
	if d > 0 {
		s.convertTimeout = d
	}
	return s
}

// Drain waits for detached conversions to finish, or for ctx to be done.
// Called at shutdown before the pools close; a conversion cut off by the
// shutdown budget leaves its key in_progress until the ledger lease expires,
// after which a retry resumes it (every step is a Keycloak-effect-first
// reconcile).
func (s *ProvisioningService) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	ctx, cancel := compensationContext(ctx)
	defer cancel()
	if err := s.keycloak.DeleteRealmIfExists(ctx, realmName); err != nil {
		s.log.WarnContext(ctx, "compensating teardown: delete realm failed", "realm", realmName, "error", err.Error())
	}
}

// platformAutomationRotationNamespace is a fixed namespace UUID used to
// derive a deterministic TS-1 rotation_id from an Idempotency-Key
// (mintRotationID) — never sent to Keycloak or Token Service itself, just
// a stable seed so a retry of the SAME failed/in-progress attempt (§9.2)
// reuses the SAME rotation_id and lands on TS-1's own idempotent replay
// path (§9.2 of the Token Service LLD) instead of minting a fresh,
// unnecessary credential rotation on every retry.
var platformAutomationRotationNamespace = uuid.MustParse("9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d")

// mintRotationID derives a deterministic TS-1 rotation_id from
// idempotencyKey so retries of the same provisioning attempt are
// idempotent end-to-end, not just at this service's own ledger.
func mintRotationID(idempotencyKey string) uuid.UUID {
	return uuid.NewSHA1(platformAutomationRotationNamespace, []byte(idempotencyKey))
}

// dedicatedPlatformAutomationClientID / sharedPlatformAutomationClientID
// name the platform-automation Keycloak client (§2.5). They live in domain
// so the reconciler's trial teardown derives the same names.
const dedicatedPlatformAutomationClientID = domain.DedicatedPlatformAutomationClientID

func sharedPlatformAutomationClientID(tenantID uuid.UUID) string {
	return domain.SharedPlatformAutomationClientID(tenantID)
}

// bestEffortDeleteServiceAccountClient deletes clientID in realmName as
// compensating cleanup (§9.3-style, scoped to just the client rather than
// the whole realm) — either an orphan left behind by a partial mint failure
// (mintPlatformAutomation) or the stale trial-realm client RP-3 conversion
// leaves behind once the principal has moved to a fresh client in the
// dedicated realm. Best-effort: the caller already has the primary result
// to return, so a teardown failure here is only logged.
func (s *ProvisioningService) bestEffortDeleteServiceAccountClient(ctx context.Context, realmName, clientID string) {
	ctx, cancel := compensationContext(ctx)
	defer cancel()
	if err := s.keycloak.DeleteServiceAccountClientIfExists(ctx, realmName, clientID); err != nil {
		s.log.WarnContext(ctx, "compensating teardown: delete service-account client failed", "realm", realmName, "client_id", clientID, "error", err.Error())
	}
}

// mintPlatformAutomation mints the tenant's platform-automation
// service-account client in realmName — authenticated via Keycloak's
// client-jwt/jwks.url mechanism against Token Service's JWKS endpoint
// (EXT-6, corrected from plain client-secret auth, which has no
// rotation-overlap mechanism) — registers it with Token Service (TS-4),
// and issues its first credential (TS-1) so the JWKS endpoint isn't empty.
// No Keycloak-side "apply" step is needed at mint time: Keycloak fetches
// the JWKS lazily on first real use, verified empirically to work with no
// pre-warming. idempotencyKey seeds a deterministic rotation_id so a retry
// of the same attempt does not mint an unnecessary fresh rotation. If
// either Token Service call fails after CreateServiceAccountClient minted a
// BRAND NEW Keycloak client this call (created == true), that client is
// best-effort deleted before returning so a caller with no realm-level
// compensating teardown (ProvisionTrial's shared realm) is not left with an
// orphaned, never-registered client; a retry of the same idempotencyKey
// then mints a fresh one rather than resuming against a client TS-4/TS-1
// never actually learned about. A RESUMED client (created == false) is
// never deleted on this signal alone — it predates this call, so it may
// already be correctly registered and actively serving traffic from an
// earlier, since-interrupted attempt; deleting it on an unrelated later
// failure would risk breaking a working credential rather than just
// cleaning up an orphan.
//
// Known gap (not fixable within this repo alone): if RegisterPrincipal
// (TS-4) succeeds — carrying Token Service's principal record over to this
// client — but IssueOrRotateCredential (TS-1) then fails and this client is
// deleted as a fresh-create orphan, Token Service is left believing the
// principal's keycloak_client_id is this now-deleted client. Token Service
// has no revert-the-carry-over API for this service to call back into; the
// window self-heals on the next successful retry of the same
// idempotencyKey (RegisterPrincipal's carry-over re-points the principal to
// whatever client that retry mints), matching this service's general
// idempotent-replay recovery model, but persists indefinitely if no retry
// ever happens.
func (s *ProvisioningService) mintPlatformAutomation(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, realmName, clientID string) (principalID uuid.UUID, err error) {
	defer func() { metrics.ServiceAccountRegisterTotal.WithLabelValues(outcome(err)).Inc() }()
	sub, created, err := s.keycloak.CreateServiceAccountClient(ctx, realmName, clientID, s.tokenService.JWKSURLFor(tenantID))
	if err != nil {
		return uuid.Nil, err
	}
	reg, err := s.tokenService.RegisterPrincipal(ctx, tenantID, port.RegisterPrincipalRequest{
		PrincipalSub: sub, KeycloakClientID: clientID,
	})
	if err != nil {
		// TS-4 failed — Token Service has no record of this client yet.
		// Safe to delete the Keycloak client as a compensating cleanup.
		if created {
			s.bestEffortDeleteServiceAccountClient(ctx, realmName, clientID)
		}
		return uuid.Nil, err
	}
	if _, err := s.tokenService.IssueOrRotateCredential(ctx, tenantID, reg.PrincipalID, port.IssueCredentialRequest{
		RotationID: mintRotationID(idempotencyKey),
	}); err != nil {
		// TS-4 already succeeded — Token Service's principal record now
		// references this Keycloak client. Do NOT delete the client here:
		// deleting it would leave Token Service with an orphaned principal
		// pointing to a gone client (worse than a never-activated-but-
		// registered client). A retry of the same idempotencyKey self-heals:
		//   1. CreateServiceAccountClient → created=false (client still exists)
		//   2. RegisterPrincipal (TS-4) → idempotent 200, same principal_id
		//   3. IssueOrRotateCredential (TS-1) → same rotation_id → replay 200
		// RP-C reconcilers handle any truly orphaned clients on their own
		// schedule if no retry ever happens — a far less harmful state than
		// a committed Token Service principal with no live Keycloak client.
		return uuid.Nil, err
	}
	return reg.PrincipalID, nil
}

// resolveUserCreateConflict handles a domain.ErrUserEmailConflict from
// CreateTrialUser/CreateInvitedUser (RP-1/RP-2/RP-5, §9.2): looks up the
// existing user by email and compares its tenant_id attribute against
// wantTenantID. A match means createErr's 409 is this caller's OWN earlier
// attempt whose success response was lost (retryTransport never retries a
// 409 itself, but the caller — e.g. an HTTP client retrying the same
// Idempotency-Key after a network blip — can) — safe to resume with the
// existing user's id as if CreateUser had returned it directly. A mismatch
// (a different tenant's user genuinely holds this email) or an
// attribute-less legacy user (TenantID stays uuid.Nil, which never equals
// a real tenant id) is a real conflict: createErr is returned unchanged so
// the caller still gets ErrUserEmailConflict, not a masked/wrong error.
func (s *ProvisioningService) resolveUserCreateConflict(ctx context.Context, realmName, email string, wantTenantID uuid.UUID, createErr error) (uuid.UUID, error) {
	if wantTenantID == uuid.Nil {
		// Guards the comment's own claim above: without this, a caller
		// reaching here with an unset TenantID (uuid.Nil) would match ANY
		// attribute-less legacy user found by FindUserByEmail — both sides
		// zero-value — and hand back that unrelated user's id as if it
		// were newly created for this (non-existent) tenant.
		return uuid.Nil, createErr
	}
	existing, err := s.keycloak.FindUserByEmail(ctx, realmName, email)
	if err != nil {
		// Couldn't confirm which case this is — surface the ORIGINAL
		// conflict rather than the lookup failure, so the caller still
		// gets a clean 409 instead of a confusing unrelated error.
		return uuid.Nil, createErr
	}
	if existing.TenantID == wantTenantID {
		return existing.ID, nil
	}
	return uuid.Nil, createErr
}

// isUserEmailConflict reports whether err is domain.ErrUserEmailConflict —
// the distinguishable signal CreateTrialUser/CreateInvitedUser produce on a
// 409 (§9.2), as opposed to every other Keycloak failure.
func isUserEmailConflict(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.ErrUserEmailConflict
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

// rejectRemovedTenant is RP-1/RP-2's guard against reusing a tenant_id whose
// identity this service already removed. It covers two cases:
//   - an offboarded row that is still visible (status exported/deleted,
//     RP-INV-5);
//   - a tombstoned row that FindByTenantID hides (deleted_at set by
//     trial-hard-delete or orphaned-signup-reap, §8.7).
//
// Both are terminal. The tombstone case matters because the scrub also
// deleted the tenant's provisioning_operations, so an RP-1 retry with the
// original Idempotency-Key is not a replay. Without this guard it would
// re-create the Keycloak user. Fails closed: a failed tombstone lookup is
// returned rather than letting provisioning proceed.
func (s *ProvisioningService) rejectRemovedTenant(ctx context.Context, tenantID uuid.UUID) error {
	existing, err := s.realms.FindByTenantID(ctx, tenantID)
	if err == nil {
		if existing.IsOffboarded() {
			return domain.NewError(domain.ErrTenantOffboarded, "tenant has been offboarded and cannot be re-provisioned")
		}
		return nil
	}
	tombstoned, terr := s.realms.IsTombstoned(ctx, tenantID)
	if terr != nil {
		return terr
	}
	if tombstoned {
		return domain.NewError(domain.ErrTenantOffboarded, "tenant has been removed and cannot be re-provisioned")
	}
	return nil
}

// enqueueTermsAccepted queues the tenant owner's terms acceptance for Audit
// Log (legal.terms.accepted, §10.6) inside the caller's RunInTx, so it
// commits or rolls back with tenant_realms. The deterministic key makes a
// replayed or resumed signup a no-op. IP address and user agent go only
// here, point to point — never on TenantIdentityProvisioned.
func (s *ProvisioningService) enqueueTermsAccepted(ctx context.Context, tenantID, ownerID uuid.UUID, terms *domain.TermsAcceptance, signupType, plan string) error {
	if s.auditEntries == nil {
		return errors.New("provisioning: no pending_audit_entries repository wired for TermsAccepted")
	}
	entry := domain.TermsAcceptedAuditEntry(tenantID, ownerID, *terms, signupType, plan, gincommon.SpanTraceID(ctx))
	return s.auditEntries.EnqueueOnce(ctx, domain.TermsAcceptedIdempotencyKey(tenantID, ownerID, terms.Version), entry)
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
	// Terms is the owner's terms-of-service acceptance (required).
	Terms *domain.TermsAcceptance
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
// TenantIdentityProvisioned. Keycloak-effect-first, then local commit (§9.1).
// Idempotent via idempotencyKey (§9.2).
func (s *ProvisioningService) ProvisionTrial(ctx context.Context, idempotencyKey string, req ProvisionTrialRequest) (out *ProvisionTrialResult, err error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, req.TenantID, domain.OpProvisionTrial)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ProvisionTrialResult](existing)
	}
	// Registered past the replay check so an idempotent retry of an
	// already-succeeded key does not inflate the attempt count.
	defer func() { metrics.ProvisionTotal.WithLabelValues("trial", outcome(err)).Inc() }()

	// After the replay check: a completed key replays its stored result
	// even once accepted_at has aged past the 24h bound.
	if err := req.Terms.Validate(time.Now()); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	// Guard: reject re-provisioning an offboarded or removed tenant before
	// touching Keycloak (RP-1, §8.1, ErrTenantOffboarded).
	if err := s.rejectRemovedTenant(ctx, req.TenantID); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	kcUserID, err := s.keycloak.CreateTrialUser(ctx, port.CreateUserRequest{
		Email: req.AdminEmail, FullName: req.AdminName, TenantID: req.TenantID,
		HashedPasswordSeed: req.HashedPasswordSeed, EmailVerified: false,
	})
	if isUserEmailConflict(err) {
		kcUserID, err = s.resolveUserCreateConflict(ctx, s.sharedRealm, req.AdminEmail, req.TenantID, err)
	}
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	// §2.5/RP-17: mint the tenant's platform-automation principal in the
	// shared trial realm before the local commit — one more Keycloak-effect
	// step in this flow's effect-first sequence (§9.1). Tenant-scoped
	// clientId: the shared realm cannot hold two clients named identically.
	automationClientID := sharedPlatformAutomationClientID(req.TenantID)
	principalID, err := s.mintPlatformAutomation(ctx, idempotencyKey, req.TenantID, s.sharedRealm, automationClientID)
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
			Status:                    domain.RealmStatusActive,
			ServiceAccountPrincipalID: &principalID, ServiceAccountKeycloakClientID: &automationClientID,
		}); err != nil {
			return err
		}
		if err := s.enqueueTermsAccepted(ctx, req.TenantID, kcUserID, req.Terms, domain.SignupTypeTrial, req.Plan); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, tenantIdentityProvisionedEvent(ctx, req.TenantID, kcUserID,
			req.TenantSlug, req.TenantName, req.Plan, domain.SignupTypeTrial, s.sharedRealm, domain.RealmStrategyShared))
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

// tenantIdentityProvisionedEvent builds the TenantIdentityProvisioned event
// (§7.5, BFF-2 signup backstop): RP-1's only event, and the first of RP-2's
// four in its commit transaction. slug/name/plan are passed through exactly as received —
// O&M's I-1 keeps whichever of this event or the BFF's own call lands
// first, so the two must never differ.
func tenantIdentityProvisionedEvent(ctx context.Context, tenantID, ownerUserID uuid.UUID, slug, name, plan, signupType, realm string, realmType domain.RealmStrategy) *domain.Event {
	return &domain.Event{
		Type: domain.EventTenantIdentityProvisioned, TenantID: tenantID,
		Subject: "tenants/" + tenantID.String(),
		Actor:   callerActor(ctx),
		Data: domain.TenantIdentityProvisionedPayload{
			TenantID: tenantID, OwnerUserID: ownerUserID, Slug: slug, Name: name, Plan: plan,
			SignupType: signupType, Realm: realm, RealmType: string(realmType),
		},
	}
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
	// Terms is the owner's terms-of-service acceptance (required).
	Terms *domain.TermsAcceptance
}

// ProvisionDirectResult is the RP-2 response body (§5.2).
//
// KeycloakUserID is the tenant owner's Keycloak `sub`, exactly as RP-1
// returns it: the Signup BFF passes it to O&M I-1 as owner_user_id, so
// both signup paths call I-1 the same way (LLD v1.42). It is the user this
// call created, or — on the user-exists conflict resume — the existing
// same-tenant user, and it is stored in the idempotency ledger so a replay
// returns it too.
type ProvisionDirectResult struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	Realm          string    `json:"realm"`
	Strategy       string    `json:"strategy"`
	KeycloakUserID uuid.UUID `json:"keycloak_user_id"`
	KeycloakShard  string    `json:"keycloak_shard"`
	OIDCClients    []string  `json:"oidc_clients"`
}

// ProvisionDirect implements RP-2 (§8.2): provision the dedicated realm
// straight away (no shared-realm intermediate), create the admin user, and
// emit TenantIdentityProvisioned + TenantRealmReady + DirectPaidSignup +
// TenantConverted(direct_signup=true).
func (s *ProvisioningService) ProvisionDirect(ctx context.Context, idempotencyKey string, req ProvisionDirectRequest) (out *ProvisionDirectResult, err error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, req.TenantID, domain.OpProvisionDirect)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ProvisionDirectResult](existing)
	}
	defer func() { metrics.ProvisionTotal.WithLabelValues("direct", outcome(err)).Inc() }()

	// After the replay check, as in RP-1.
	if err := req.Terms.Validate(time.Now()); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	// Guard: reject re-provisioning an offboarded or removed tenant before
	// touching Keycloak (RP-2, §8.2, ErrTenantOffboarded).
	if err := s.rejectRemovedTenant(ctx, req.TenantID); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
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
	kcUserID, err := s.keycloak.CreateInvitedUser(ctx, realmName, port.CreateUserRequest{
		Email: req.AdminEmail, FullName: req.AdminName, TenantID: req.TenantID,
		HashedPasswordSeed: req.HashedPasswordSeed, EmailVerified: false,
		RequiredActions: []string{"VERIFY_EMAIL"},
	})
	if isUserEmailConflict(err) {
		// A lost-response retry of THIS SAME request lands here with the
		// realm already fully provisioned — resolve in place rather than
		// falling through to the compensating delete below, which would
		// otherwise destroy and recreate a perfectly good realm+OIDC
		// clients just to dodge an email that's actually this tenant's own
		// already-created user (§9.2).
		kcUserID, err = s.resolveUserCreateConflict(ctx, realmName, req.AdminEmail, req.TenantID, err)
	}
	if err != nil {
		s.bestEffortDeleteRealm(ctx, realmName)
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	// §2.5/RP-17: mint the tenant's platform-automation principal in the
	// new dedicated realm before the local commit — same compensating
	// teardown as the two Keycloak-effect steps above on failure.
	principalID, err := s.mintPlatformAutomation(ctx, idempotencyKey, req.TenantID, realmName, dedicatedPlatformAutomationClientID)
	if err != nil {
		s.bestEffortDeleteRealm(ctx, realmName)
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	result := &ProvisionDirectResult{
		TenantID: req.TenantID, Realm: realmName, Strategy: string(domain.RealmStrategyDedicated),
		KeycloakUserID: kcUserID, KeycloakShard: shard, OIDCClients: clients,
	}

	automationClientID := dedicatedPlatformAutomationClientID
	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		if err := s.realms.Upsert(ctx, &domain.TenantRealm{
			TenantID: req.TenantID, RealmName: realmName, Strategy: domain.RealmStrategyDedicated,
			KeycloakShard: &shard, Status: domain.RealmStatusActive, ConvertedAt: nowPtr(),
			ServiceAccountPrincipalID: &principalID, ServiceAccountKeycloakClientID: &automationClientID,
		}); err != nil {
			return err
		}
		if err := s.enqueueTermsAccepted(ctx, req.TenantID, kcUserID, req.Terms, domain.SignupTypeDirectPaid, req.Plan); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		// First, so O&M's backstop normally sees the tenant before the
		// realm-ready/settle events below (outbox order only — the topic is
		// not FIFO, so O&M still tolerates any arrival order).
		if err := pub.Enqueue(ctx, tenantIdentityProvisionedEvent(ctx, req.TenantID, kcUserID,
			req.TenantSlug, req.TenantName, req.Plan, domain.SignupTypeDirectPaid, realmName, domain.RealmStrategyDedicated)); err != nil {
			return err
		}
		if err := pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventTenantRealmReady, TenantID: req.TenantID,
			Subject: "tenants/" + req.TenantID.String(),
			Data: domain.TenantRealmReadyPayload{
				TenantID: req.TenantID, Realm: realmName, KeycloakShard: shard, OIDCClients: clients,
			},
		}); err != nil {
			return err
		}
		if err := pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventDirectPaidSignup, TenantID: req.TenantID,
			Subject: "tenants/" + req.TenantID.String(),
			Data: domain.DirectPaidSignupPayload{
				TenantID: req.TenantID, Realm: realmName, Plan: req.Plan, DirectSignup: true,
			},
		}); err != nil {
			return err
		}
		var billingSubID *string
		if req.BillingSubscriptionID != "" {
			billingSubID = &req.BillingSubscriptionID
		}
		return pub.Enqueue(ctx, &domain.Event{
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

type convertOutcome struct {
	out *ConvertResult
	err error
}

// Convert implements RP-3 (§8.3): trial->paid conversion with user
// migration. Requires the tenant to currently be `shared` strategy.
//
// Conversion is O(users) Keycloak calls (§11.1: p95 ≤ 8s + ~0.3s/user), so
// for a large trial it can outrun the 30s request deadline. Once the key is
// claimed the work therefore runs on a context detached from the request,
// bounded by convertTimeout. If the request is done first (deadline or
// client disconnect) the caller gets 409 operation_in_progress — the
// existing contract tells it to retry with the same Idempotency-Key — and
// the conversion keeps going; the retry replays its stored result once it
// has finished. Canceling half-way through a user migration would instead
// leave a partially-migrated dedicated realm behind.
func (s *ProvisioningService) Convert(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, req ConvertRequest) (*ConvertResult, error) {
	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpConvert)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ConvertResult](existing)
	}

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.convertTimeout)
	done := make(chan convertOutcome, 1)
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(runCtx, "convert: panic in detached conversion", "tenant_id", tenantID, "panic", fmt.Sprint(r))
				s.failIdempotency(runCtx, idempotencyKey)
				done <- convertOutcome{err: fmt.Errorf("convert: panic: %v", r)}
			}
		}()
		out, err := s.convertClaimed(runCtx, idempotencyKey, tenantID, req)
		done <- convertOutcome{out: out, err: err}
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return nil, domain.NewError(domain.ErrOperationInProgress, "conversion is still running; retry with the same Idempotency-Key").
			WithDetails(map[string]any{"tenant_id": tenantID})
	}
}

// convertClaimed is the RP-3 conversion body, run after Convert has claimed
// idempotencyKey.
func (s *ProvisioningService) convertClaimed(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, req ConvertRequest) (out *ConvertResult, err error) {
	defer func() { metrics.ProvisionTotal.WithLabelValues("convert", outcome(err)).Inc() }()

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
	// derives a stable name from the tenant id. Uses the FULL uuid, not a
	// truncated prefix: the realm-name collision below is now treated as
	// proof of "my own earlier partial attempt" and resumed against
	// (rather than always a hard error), so an 8-hex-char (32-bit) prefix
	// would let two different tenants collide onto the same realm at
	// large-enough scale and have the second tenant's users migrated into
	// the first tenant's realm — a tenant-isolation violation. The full
	// UUID carries the same 128 bits of collision resistance this service
	// already relies on for tenant-id uniqueness itself.
	slug := tenantID.String()
	realmName := dedicatedRealmName(slug)
	shard, err := s.selectShard(ctx)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	// realmMayHaveMigratedUsers gates every compensating teardown below
	// (§9.3): MigrateUser deletes each user from the source realm before
	// creating it in the target, so once this is true, realmName holds
	// users that no longer exist anywhere else — deleting the realm at
	// that point would destroy them permanently, not just roll back this
	// attempt. It is set once, either because CreateRealmFromTemplate's
	// conflict proves an earlier attempt already migrated users into this
	// same realm (see below), or because this attempt migrates one itself.
	realmMayHaveMigratedUsers := false

	if err := s.keycloak.CreateRealmFromTemplate(ctx, realmName, "v1"); err != nil {
		var domainErr *domain.Error
		if !errors.As(err, &domainErr) || domainErr.Code != domain.ErrRealmAlreadyDedicated {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, err
		}
		// realmName is deterministic per tenant (dedicatedRealmName), and
		// current.Strategy read above is still shared, so this conflict
		// cannot be a different tenant's realm — it can only be this
		// tenant's own earlier attempt that got partway through this same
		// Convert call before failing. Given the teardown gating below, the
		// only way an earlier attempt can have left the realm behind on
		// failure is if it had already migrated at least one user into it,
		// so resuming here must treat the realm as unsafe to delete too.
		realmMayHaveMigratedUsers = true
		s.log.WarnContext(ctx, "convert: target realm already exists, resuming a prior partial attempt", "tenant_id", tenantID, "realm", realmName)
	}
	clients, err := s.keycloak.RegisterOIDCClients(ctx, realmName, slug)
	if err != nil {
		if !realmMayHaveMigratedUsers {
			s.bestEffortDeleteRealm(ctx, realmName)
		}
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrConversionFailed, "register oidc clients failed").WithDetails(map[string]any{"error": err.Error()})
	}

	// §2.5/RP-17: carry the platform-automation principal across into the
	// new dedicated realm — the trial-realm client cannot itself move
	// realms, so this mints a fresh dedicated-realm client and re-registers
	// it against the SAME Token Service principal (TS-4's carry-over
	// upsert, §25 rev 1.1 of the Token Service LLD, updates principal_sub
	// in place rather than creating a second principal). Placed here,
	// before the user-migration loop begins, so a failure can still safely
	// trigger the realmMayHaveMigratedUsers-gated teardown below.
	principalID, err := s.mintPlatformAutomation(ctx, idempotencyKey, tenantID, realmName, dedicatedPlatformAutomationClientID)
	if err != nil {
		if !realmMayHaveMigratedUsers {
			s.bestEffortDeleteRealm(ctx, realmName)
		}
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrConversionFailed, "mint platform-automation principal failed").WithDetails(map[string]any{"error": err.Error()})
	}

	sourceUsers, err := s.keycloak.ListUsersByTenant(ctx, current.RealmName, tenantID)
	if err != nil {
		if !realmMayHaveMigratedUsers {
			s.bestEffortDeleteRealm(ctx, realmName)
		}
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrConversionFailed, "list trial users failed").WithDetails(map[string]any{"error": err.Error()})
	}

	webauthnReenroll := 0
	for _, uid := range sourceUsers {
		reenroll, err := s.keycloak.MigrateUser(ctx, current.RealmName, realmName, uid)
		if err != nil {
			// Leave the partially-populated realm in place instead of
			// tearing it down whenever realmMayHaveMigratedUsers; a retry
			// with the same Idempotency-Key hits the ErrRealmAlreadyDedicated
			// branch above, resumes against it, and a fresh
			// ListUsersByTenant naturally picks up only whatever remains
			// in the shared realm.
			if !realmMayHaveMigratedUsers {
				s.bestEffortDeleteRealm(ctx, realmName)
			}
			s.failIdempotency(ctx, idempotencyKey)
			return nil, domain.NewError(domain.ErrConversionFailed, "migrate user failed").WithDetails(map[string]any{"error": err.Error(), "keycloak_user_id": uid})
		}
		realmMayHaveMigratedUsers = true
		if reenroll {
			webauthnReenroll++
		}
	}
	metrics.UsersMigratedTotal.Add(float64(len(sourceUsers)))

	if err := s.keycloak.ForceLogoutRealm(ctx, current.RealmName); err != nil {
		s.log.WarnContext(ctx, "convert: force_logout_realm failed (non-fatal, sessions expire naturally)", "tenant_id", tenantID, "error", err.Error())
	}

	result := &ConvertResult{
		TenantID: tenantID, Realm: realmName, Strategy: string(domain.RealmStrategyDedicated),
		KeycloakShard: shard, OIDCClients: clients, UsersMigrated: len(sourceUsers),
		WebauthnReenrollRequired: webauthnReenroll, SharedSessionsTerminated: true,
	}

	// Captured before the tx closure below overwrites current.RealmName/
	// current.ServiceAccountKeycloakClientID with the new dedicated realm's
	// — needed after a successful commit to retire the shared-realm client
	// the principal has now moved off of (§2.5). Reads the persisted
	// client id rather than re-deriving it via sharedPlatformAutomationClientID:
	// current.ServiceAccountKeycloakClientID is what Convert actually stored
	// as this tenant's shared-realm client at mint time, so this stays
	// correct even if the naming convention ever changes or a row was
	// populated out-of-band; falls back to the derived name only for a
	// legacy/never-minted row where the field is nil.
	oldRealmName := current.RealmName
	oldClientID := sharedPlatformAutomationClientID(tenantID)
	if current.ServiceAccountKeycloakClientID != nil {
		oldClientID = *current.ServiceAccountKeycloakClientID
	}
	// Captured before the tx closure overwrites current.ServiceAccountPrincipalID.
	// nil for a legacy row provisioned before mintPlatformAutomation was added.
	oldPrincipalID := current.ServiceAccountPrincipalID

	automationClientID := dedicatedPlatformAutomationClientID
	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		current.Strategy = domain.RealmStrategyDedicated
		current.RealmName = realmName
		current.KeycloakShard = &shard
		current.Status = domain.RealmStatusActive
		current.ConvertedAt = nowPtr()
		current.ServiceAccountPrincipalID = &principalID
		current.ServiceAccountKeycloakClientID = &automationClientID
		if err := s.realms.Update(ctx, current); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		if err := pub.Enqueue(ctx, &domain.Event{
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
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventTenantConverted, TenantID: tenantID,
			Subject: "tenants/" + tenantID.String(),
			Actor:   callerActor(ctx),
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

	// TS-2: revoke the old shared-realm credential now that the local commit
	// has landed and the new dedicated-realm credential (TS-1, issued inside
	// mintPlatformAutomation above) is live (RP-GAP-004/TS-RP-GAP-002).
	// Best-effort: a revocation failure leaves the old credential active until
	// RP-C reconcilers or operator cleanup, which is far less harmful than
	// blocking an already-committed conversion. Must run AFTER TS-1 (which
	// issued the new credential) and BEFORE deleting the old Keycloak client
	// below — the strict TS-4 → TS-1 → TS-2 → client-delete ordering that
	// avoids any window where the principal has no valid credential.
	// Skipped for legacy rows that were provisioned before mintPlatformAutomation
	// was added (oldPrincipalID == nil).
	if oldPrincipalID != nil {
		if tsErr := s.tokenService.RevokeCredential(ctx, tenantID, *oldPrincipalID); tsErr != nil {
			s.log.WarnContext(ctx, "convert: revoke old shared-realm credential failed (non-fatal)", "tenant_id", tenantID, "error", tsErr.Error())
		}
	}

	// The conversion has fully committed — the principal now lives on
	// realmName's client, so the old shared-realm client is retired
	// (best-effort; a failure here leaves a harmless cleanup item, not an
	// inconsistency, since the committed row already points at the new
	// client). Synchronous, adding one Keycloak round-trip to this
	// response — deliberately, mirroring ForceLogoutRealm's identical
	// non-fatal-but-synchronous shape earlier in this same method, so a
	// completed HTTP response always reflects every best-effort side
	// effect this call attempted, not just the ones an async worker
	// happened to get to first.
	s.bestEffortDeleteServiceAccountClient(ctx, oldRealmName, oldClientID)

	body := marshalIdempotencyResult(ctx, s.log, "convert", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "convert: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// RefreshServiceAccountKeys implements RP-17's standalone rotate/revoke/
// break-glass path (§2.5, corrected 2026-09-18/EXT-6 from an "apply a
// caller-supplied secret" shape): the caller (operator/O&M tooling, or the
// caller-facing side of a rotate/revoke) has already issued or revoked a
// credential directly against Token Service's own TS-1/TS-2, and calls this
// so the tenant's realm actually notices — Keycloak does not self-refresh a
// jwks.url client's cached keys, verified empirically, so a new key is
// unrecognized and a removed key keeps authenticating until this runs.
// Serves both the rotate-apply and the revoke-enforce case with one action.
// No RunInTx/outbox — this mutates no tracked local field, it is a pure
// Keycloak-effect call (closer to RP-9/RP-15/RP-16's shape than
// RP-1/2/3's). Fail-closed on a Keycloak-unreachable error rather than
// RP-8's local-intent-first reconciler pattern: a stale key cache is
// security-sensitive (a delayed reconciler retry risks leaving a
// compromised key live longer than necessary), and the caller can simply
// retry the call itself. Realm-scoped, not client-scoped: for RP-1's shared
// trial realm this refreshes every trial tenant's automation client
// sharing that realm — harmless (extra re-fetches only), not optimized for
// in v1.
func (s *ProvisioningService) RefreshServiceAccountKeys(ctx context.Context, tenantID uuid.UUID) (err error) {
	defer func() { metrics.ServiceAccountKeyRefreshTotal.WithLabelValues(outcome(err)).Inc() }()
	realm, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return err
	}
	if realm.ServiceAccountKeycloakClientID == nil {
		return domain.NewError(domain.ErrServiceAccountNotProvisioned, "tenant has no platform-automation principal minted yet").
			WithDetails(map[string]any{"tenant_id": tenantID})
	}
	return s.keycloak.ClearServiceAccountKeysCache(ctx, realm.RealmName)
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
	if isUserEmailConflict(err) {
		userID, err = s.resolveUserCreateConflict(ctx, row.RealmName, req.Email, tenantID, err)
	}
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
// operator runbook, §24).
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
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.ErrTenantRealmNotFound {
			// The dedicated realm no longer exists in Keycloak, even
			// though the local row (current, read above) still says
			// Strategy=dedicated — this can only mean an earlier attempt
			// with this same Idempotency-Key already migrated every user
			// and deleted the realm (§9.1: Keycloak-effect-first) but
			// failed before the local commit below landed. There is
			// nothing left to migrate or delete; resume straight to
			// fixing up the local row.
			s.log.WarnContext(ctx, "revert_conversion: dedicated realm already gone, resuming a prior partial attempt", "tenant_id", tenantID, "realm", dedicatedRealm)
		} else {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, domain.NewError(domain.ErrConversionFailed, "list dedicated-realm users failed").WithDetails(map[string]any{"error": err.Error()})
		}
	} else {
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
	}

	// §2.5/RP-17: the dedicated realm just torn down (or already gone on a
	// resumed attempt) took its "platform-automation" client with it, so the
	// tenant is left with no working principal at all once back on the
	// shared realm — mint a fresh tenant-scoped client there and carry the
	// SAME Token Service principal over to it (TS-4's carry-over upsert),
	// mirroring Convert's mint-on-the-way-in. Without this, the local row
	// would keep pointing at a client/realm that no longer exists, and
	// RP-17's RefreshServiceAccountKeys would silently no-op against it
	// instead of surfacing the tenant's missing credential.
	sharedClientID := sharedPlatformAutomationClientID(tenantID)
	principalID, err := s.mintPlatformAutomation(ctx, idempotencyKey, tenantID, s.sharedRealm, sharedClientID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrConversionFailed, "mint platform-automation principal failed").WithDetails(map[string]any{"error": err.Error()})
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
		current.ServiceAccountPrincipalID = &principalID
		current.ServiceAccountKeycloakClientID = &sharedClientID
		if err := s.realms.Update(ctx, current); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventTrialReactivated, TenantID: tenantID,
			Actor: callerActor(ctx),
			Data:  domain.TrialReactivatedPayload{TenantID: tenantID, NewTrialEndsAt: newTrialEndsAt.Format(time.RFC3339)},
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
	return nil, domain.NewError(domain.ErrOperationInProgress, "a call with this Idempotency-Key is already in progress — retry with the same key")
}
