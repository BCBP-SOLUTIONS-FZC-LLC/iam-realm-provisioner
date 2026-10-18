package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// validIdpAlias restricts idp_alias to a safe charset — letters, digits,
// hyphen, underscore, 1-64 chars. This is this service's only guard
// against a caller-supplied alias containing a path-traversal sequence
// (e.g. "../"): openbao.Client.idpPath builds
// "<prefix>/<tenant_id>/<idp_alias>" via plain string interpolation, with
// no sanitization at that layer, and idp_alias otherwise flows there
// unvalidated all the way from RP-6's JSON body / RP-7's URL path param.
// No dot is permitted at all, so "." and ".." can never appear as a
// component.
var validIdpAlias = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func validateIdpAliasFormat(alias string) error {
	if !validIdpAlias.MatchString(alias) {
		return domain.NewError(domain.ErrInvalidRequest, "idp_alias must match ^[a-zA-Z0-9_-]{1,64}$").
			WithDetails(map[string]any{"idp_alias": alias})
	}
	return nil
}

// IdpService implements RP-6/RP-7/RP-13 (§8.4/8.5, RP-INV-4 — the §11.6
// gate). Every successful IdP change also records a TenantIdpConfigChanged
// audit entry in pending_audit_entries, in the same transaction as the
// keycloak_idp_configs write, for AL-5 direct-write delivery (§10.6).
type IdpService struct {
	realms       port.TenantRealmRepository
	idpConfigs   port.IdpConfigRepository
	validations  port.IdpValidationRepository
	idempotency  port.IdempotencyStore
	openbao      port.OpenBaoClient
	keycloak     port.KeycloakAdmin
	tx           port.TxRunner
	auditEntries port.PendingAuditEntryRepository
	log          port.SlogStyleLogger
}

// NewIdpService wires IdpService's dependencies.
func NewIdpService(
	realms port.TenantRealmRepository,
	idpConfigs port.IdpConfigRepository,
	validations port.IdpValidationRepository,
	idempotency port.IdempotencyStore,
	openbao port.OpenBaoClient,
	keycloak port.KeycloakAdmin,
	tx port.TxRunner,
	auditEntries port.PendingAuditEntryRepository,
) *IdpService {
	return &IdpService{
		realms: realms, idpConfigs: idpConfigs, validations: validations, idempotency: idempotency,
		openbao: openbao, keycloak: keycloak, tx: tx, auditEntries: auditEntries,
	}
}

// WithLogger attaches a logger and returns s for chaining.
func (s *IdpService) WithLogger(log port.Logger) *IdpService {
	s.log = port.NewSlogStyleLogger(log)
	return s
}

// failIdempotency marks idempotencyKey's provisioning_operations row
// failed. This is a best-effort side effect on an already-failing path —
// the original error is what the caller returns regardless of whether this
// succeeds, so a failure here is only logged.
func (s *IdpService) failIdempotency(ctx context.Context, idempotencyKey string) {
	if err := s.idempotency.Fail(ctx, idempotencyKey); err != nil {
		s.log.WarnContext(ctx, "idempotency: mark failed did not persist", "idempotency_key", idempotencyKey, "error", err.Error())
	}
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
// OpenBao then creates/updates the Keycloak IdP. Ledgered against
// idempotencyKey like every other mutating RP endpoint (§9.2) — the route
// already requires the header (RequireIdempotencyKey), this is what backs
// that contract with an actual dedup/replay.
func (s *IdpService) ConfigureIdp(ctx context.Context, idempotencyKey string, tenantID uuid.UUID, req ConfigureIdpRequest) (*ConfigureIdpResult, error) {
	if err := validateIdpAliasFormat(req.IdpAlias); err != nil {
		return nil, err
	}

	existing, found, err := s.idempotency.Begin(ctx, idempotencyKey, tenantID, domain.OpConfigureIdp)
	if err != nil {
		return nil, err
	}
	if found {
		return replayOrConflict[ConfigureIdpResult](existing)
	}

	realm, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}
	if realm.IsOffboarded() {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrTenantOffboarded, "tenant has been offboarded")
	}
	if !realm.IsDedicated() {
		s.failIdempotency(ctx, idempotencyKey)
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
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrIdpRegionPortabilityFailed, "one or more §11.6 CRITICAL checks failed").
			WithDetails(map[string]any{"validator_version": ValidatorVersion, "failed_checks": validation.Failed, "warnings": validation.Warned})
	}
	if validation.Outcome == domain.ValidationOutcomeWarningsOnly {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, domain.NewError(domain.ErrIdpWarningsUnacknowledged, "one or more §11.6 WARN checks require acknowledgement").
			WithDetails(map[string]any{"warnings": validation.Warned})
	}

	// isNewConfig gates the compensating OpenBao-secret delete below: an
	// update to an already-configured alias must never delete the secret
	// on a later failure — the pre-existing Keycloak IdP is still live and
	// completely unaffected by a failed CreateOrUpdateIdp call below, so
	// its secret must stay in OpenBao even though WriteIdpSecret already
	// overwrote OpenBao's copy with the new (never-applied) value. Any
	// lookup error other than a confirmed not-found is treated the same
	// as "might be an update" — failing safe, since leaving a possibly
	// orphaned secret is far cheaper than destroying a live one.
	isNewConfig := false
	if _, findErr := s.idpConfigs.FindByAlias(ctx, tenantID, req.IdpAlias); findErr != nil {
		var de *domain.Error
		if errors.As(findErr, &de) && de.Code == domain.ErrIdpConfigNotFound {
			isNewConfig = true
		} else {
			s.log.WarnContext(ctx, "configure_idp: existing config lookup failed, treating as update for compensating-delete safety", "tenant_id", tenantID, "idp_alias", req.IdpAlias, "error", findErr.Error())
		}
	}

	if req.Secret != "" {
		if err := s.openbao.WriteIdpSecret(ctx, tenantID, req.IdpAlias, map[string]any{"secret": req.Secret}); err != nil {
			s.failIdempotency(ctx, idempotencyKey)
			return nil, err
		}
	}
	if err := s.keycloak.CreateOrUpdateIdp(ctx, realm.RealmName, port.IdpKeycloakConfig{
		Alias: req.IdpAlias, Protocol: string(req.Protocol), DisplayName: req.DisplayName,
		Enabled: true, Config: req.Config, Secret: req.Secret,
	}); err != nil {
		if req.Secret != "" && isNewConfig {
			// Compensating cleanup (§9.3): this alias had no prior config,
			// so the secret just written was never applied to any live
			// Keycloak IdP — best-effort delete it rather than leave an
			// orphaned secret in OpenBao with nothing referencing it.
			cctx, cancel := compensationContext(ctx)
			delErr := s.openbao.DeleteIdpSecret(cctx, tenantID, req.IdpAlias)
			cancel()
			if delErr != nil {
				s.log.WarnContext(ctx, "configure_idp: compensating openbao secret delete failed", "tenant_id", tenantID, "idp_alias", req.IdpAlias, "error", delErr.Error())
			}
		}
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	action := domain.AuditActionUpdate
	if isNewConfig {
		action = domain.AuditActionCreate
	}
	entry := idpAuditEntry(ctx, tenantID, req.IdpAlias, action, map[string]any{
		"protocol":               string(req.Protocol),
		"validator_version":      ValidatorVersion,
		"validation_outcome":     string(validation.Outcome),
		"checks":                 checksJSON,
		"acknowledged_check_ids": req.Acknowledgements,
		"secret_rotated":         req.Secret != "",
	})
	if err := s.tx.RunInTx(ctx, func(ctx context.Context) error {
		if err := s.idpConfigs.Upsert(ctx, &domain.IdpConfig{
			TenantID: tenantID, IdpAlias: req.IdpAlias, Protocol: req.Protocol,
			DisplayName: req.DisplayName, Enabled: true, Config: req.Config,
			Status: domain.IdpConfigStatusActive,
		}); err != nil {
			return err
		}
		return s.auditEntries.Enqueue(ctx, entry)
	}); err != nil {
		s.failIdempotency(ctx, idempotencyKey)
		return nil, err
	}

	result := &ConfigureIdpResult{IdpAlias: req.IdpAlias, Status: string(domain.IdpConfigStatusActive)}
	body := marshalIdempotencyResult(ctx, s.log, "configure_idp", result)
	if err := s.idempotency.Complete(ctx, idempotencyKey, body); err != nil {
		s.log.WarnContext(ctx, "configure_idp: idempotency complete failed", "tenant_id", tenantID, "error", err.Error())
	}
	return result, nil
}

// RemoveIdp implements RP-7: removes the Keycloak IdP, the OpenBao secret,
// and the local row (with its TenantIdpConfigChanged audit entry, in one
// transaction). Idempotent.
func (s *IdpService) RemoveIdp(ctx context.Context, tenantID uuid.UUID, idpAlias string) error {
	if err := validateIdpAliasFormat(idpAlias); err != nil {
		return err
	}

	realm, err := s.realms.FindByTenantID(ctx, tenantID)
	if err != nil {
		return err
	}
	cfg, err := s.idpConfigs.FindByAlias(ctx, tenantID, idpAlias)
	if err != nil {
		return err
	}
	if err := s.keycloak.DeleteIdp(ctx, realm.RealmName, idpAlias); err != nil {
		return err
	}
	if err := s.openbao.DeleteIdpSecret(ctx, tenantID, idpAlias); err != nil {
		s.log.WarnContext(ctx, "remove_idp: openbao secret delete failed (best-effort)", "tenant_id", tenantID, "idp_alias", idpAlias, "error", err.Error())
	}
	entry := idpAuditEntry(ctx, tenantID, idpAlias, domain.AuditActionDelete, map[string]any{"protocol": string(cfg.Protocol)})
	return s.tx.RunInTx(ctx, func(ctx context.Context) error {
		if err := s.idpConfigs.Delete(ctx, tenantID, idpAlias); err != nil {
			return err
		}
		return s.auditEntries.Enqueue(ctx, entry)
	})
}

// auditMetadataMaxBytes mirrors AL-5's MAX_METADATA_BYTES (NormalizeMetadata).
// Metadata exceeding the cap causes AL to return 422, which the delivery
// drainer treats as permanent (ErrAuditEntryRejected → MarkFailed). A
// fallback marker is substituted so the audit entry is never silently lost.
const auditMetadataMaxBytes = 8192

// idpAuditEntry builds the TenantIdpConfigChanged AL-5 entry (HLD §9.4,
// §10.6). The actor is the mesh caller — normally the iam-system principal
// O&M calls as — and trace_id is captured now, since delivery happens
// later from the drainer on an unrelated trace.
func idpAuditEntry(ctx context.Context, tenantID uuid.UUID, alias, action string, metadata map[string]any) domain.AuditEntry {
	metaJSON, _ := json.Marshal(metadata) //nolint:errcheck // map[string]any with plain serialisable values: cannot fail
	if len(metaJSON) > auditMetadataMaxBytes {
		metaJSON, _ = json.Marshal(map[string]any{"_truncated": true}) //nolint:errcheck // static literal map: cannot fail
	}
	entry := domain.AuditEntry{
		TenantID: tenantID, EntryType: domain.AuditEntryTypeIdpChanged, Action: action,
		Actor:      domain.AuditActor{Type: domain.AuditActorIAMSystem, ID: domain.SystemActorID, Display: domain.Source},
		Target:     &domain.AuditTarget{Type: "idp", ID: alias},
		OccurredAt: time.Now().UTC(), SourceService: domain.Source,
		SourceEventType: domain.AuditSourceTenantIdpConfigChanged,
		Metadata:        json.RawMessage(metaJSON),
	}
	if rc, ok := requestctx.FromContext(ctx); ok {
		if rc.UserID != uuid.Nil && rc.UserID.String() != domain.SystemActorID {
			entry.Actor = domain.AuditActor{Type: domain.AuditActorUser, ID: rc.UserID.String()}
		}
		entry.IPAddress, entry.UserAgent = rc.ClientIP, rc.UserAgent
	}
	if tid := gincommon.SpanTraceID(ctx); tid != "" {
		entry.TraceID = tid
	}
	return entry
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
