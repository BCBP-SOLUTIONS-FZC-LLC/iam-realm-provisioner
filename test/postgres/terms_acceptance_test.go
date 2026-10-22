//go:build integration

// Signup BFF open gap A-4 (LLD v1.41, §8.1/§8.2/§10.6): RP-1/RP-2 enqueue the
// owner's TermsAccepted audit entry in pending_audit_entries in the same
// transaction as tenant_realms, under a deterministic AL-5 key, so a replay
// or a resumed signup never queues a second one.
package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

// signupKeycloak is the Keycloak fake for a happy RP-1: CreateTrialUser
// returns the fixed owner id (also on a resumed signup, as the 409
// conflict-resume path would) and the automation client is minted.
type signupKeycloak struct {
	fakeJobsKeycloakAdmin
	owner uuid.UUID
}

func (k *signupKeycloak) CreateTrialUser(context.Context, port.CreateUserRequest) (uuid.UUID, error) {
	return k.owner, nil
}

func (k *signupKeycloak) CreateServiceAccountClient(context.Context, string, string, string) (uuid.UUID, bool, error) {
	return uuid.New(), true, nil
}

// signupTokenService registers the automation principal and issues its
// first credential.
type signupTokenService struct{ fakeJobsTokenService }

func (signupTokenService) JWKSURLFor(uuid.UUID) string { return "http://ts/jwks" }

func (signupTokenService) RegisterPrincipal(context.Context, uuid.UUID, port.RegisterPrincipalRequest) (*port.RegisterPrincipalResult, error) {
	return &port.RegisterPrincipalResult{PrincipalID: uuid.New(), Created: true}, nil
}

func (signupTokenService) IssueOrRotateCredential(context.Context, uuid.UUID, uuid.UUID, port.IssueCredentialRequest) (*port.IssueCredentialResult, error) {
	return &port.IssueCredentialResult{Version: 1}, nil
}

// failingPublisher fails the outbox enqueue, after the audit entry was
// enqueued in the same transaction.
type failingPublisher struct{}

func (failingPublisher) Enqueue(context.Context, *domain.Event) error {
	return errors.New("outbox down")
}

type pendingRow struct {
	Key   string
	Entry domain.AuditEntry
}

func TestTermsAccepted_EnqueuedWithRP1(t *testing.T) {
	t.Parallel()
	registerReapMetrics() // RP-1 counts ProvisionTotal
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	realms := pgadapter.NewTenantRealmRepository(appPool)
	idem := pgadapter.NewIdempotencyRepository(appPool)
	audit := pgadapter.NewPendingAuditEntryRepository(appPool)

	rows := func(t *testing.T, tenantID uuid.UUID) []pendingRow {
		t.Helper()
		r, err := rawPool.Query(ctx, `SELECT idempotency_key, entry FROM pending_audit_entries WHERE tenant_id = $1`, tenantID)
		require.NoError(t, err)
		defer r.Close()
		var out []pendingRow
		for r.Next() {
			var p pendingRow
			var body []byte
			require.NoError(t, r.Scan(&p.Key, &body))
			require.NoError(t, json.Unmarshal(body, &p.Entry))
			out = append(out, p)
		}
		require.NoError(t, r.Err())
		return out
	}
	request := func(tenantID uuid.UUID, terms *domain.TermsAcceptance) service.ProvisionTrialRequest {
		return service.ProvisionTrialRequest{
			TenantID: tenantID, TenantName: "Acme", TenantSlug: "acme-" + tenantID.String()[:8],
			AdminEmail: "a@acme.example", AdminName: "A", Plan: "pro", Terms: terms,
		}
	}

	t.Run("CommitsWithTenantRealmsUnderTheDeterministicKey", func(t *testing.T) {
		owner, tenantID := uuid.New(), uuid.New()
		svc := service.NewProvisioningService(realms, idem, &signupKeycloak{owner: owner}, &signupTokenService{},
			pgadapter.NewTxRunner(appPool, nil), audit, "trial", nil)
		terms := validTerms()
		_, err := svc.ProvisionTrial(withTenant(ctx, tenantID), "rp1-"+tenantID.String(), request(tenantID, terms))
		require.NoError(t, err)

		got := rows(t, tenantID)
		require.Len(t, got, 1)
		assert.Equal(t, "TermsAccepted:"+tenantID.String()+":"+owner.String()+":2026-09", got[0].Key)
		e := got[0].Entry
		assert.Equal(t, "legal.terms.accepted", e.EntryType)
		assert.Equal(t, "TermsAccepted", e.SourceEventType)
		assert.Equal(t, "iam-realm-provisioner", e.SourceService)
		assert.Equal(t, domain.AuditActor{Type: "user", ID: owner.String()}, e.Actor)
		assert.Equal(t, &domain.AuditTarget{Type: "tenant", ID: tenantID.String()}, e.Target)
		assert.True(t, terms.AcceptedAt.Equal(e.OccurredAt))
		assert.Equal(t, "203.0.113.7", e.IPAddress)
		assert.Equal(t, terms.UserAgent, e.UserAgent)
		assert.JSONEq(t, `{"terms_version":"2026-09","accepted_at":"`+terms.AcceptedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")+`","signup_type":"trial","plan":"pro"}`, string(e.Metadata))

		// A resumed signup (the user-exists conflict path, or a retry under
		// a new Idempotency-Key) with a different accepted_at for the same
		// version still holds exactly one entry — the first wins.
		later := validTerms()
		later.AcceptedAt = terms.AcceptedAt.Add(30 * time.Second)
		_, err = svc.ProvisionTrial(withTenant(ctx, tenantID), "rp1-retry-"+tenantID.String(), request(tenantID, later))
		require.NoError(t, err)
		got = rows(t, tenantID)
		require.Len(t, got, 1)
		assert.True(t, terms.AcceptedAt.Equal(got[0].Entry.OccurredAt))

		// An idempotent replay of the original key is answered from the
		// ledger and enqueues nothing either.
		_, err = svc.ProvisionTrial(withTenant(ctx, tenantID), "rp1-"+tenantID.String(), request(tenantID, terms))
		require.NoError(t, err)
		assert.Len(t, rows(t, tenantID), 1)
	})

	t.Run("RolledBackWithTheTransaction", func(t *testing.T) {
		owner, tenantID := uuid.New(), uuid.New()
		svc := service.NewProvisioningService(realms, idem, &signupKeycloak{owner: owner}, &signupTokenService{},
			pgadapter.NewTxRunner(appPool, failingPublisher{}), audit, "trial", nil)
		_, err := svc.ProvisionTrial(withTenant(ctx, tenantID), "rp1-"+tenantID.String(), request(tenantID, validTerms()))
		require.Error(t, err)
		assert.Empty(t, rows(t, tenantID), "the TermsAccepted entry must roll back with tenant_realms")
		var n int
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM tenant_realms WHERE tenant_id = $1`, tenantID).Scan(&n))
		assert.Zero(t, n)
	})
}

func TestPendingAuditEntryRepository_EnqueueOnce(t *testing.T) {
	t.Parallel()
	appPool, rawPool := setupTestDB(t)
	sysPool := setupSysPool(t, rawPool)
	ctx := context.Background()
	repo := pgadapter.NewPendingAuditEntryRepository(appPool)
	sysRepo := pgadapter.NewPendingAuditEntryRepository(sysPool)

	tenantID := uuid.New()
	terms := validTerms()
	entry := domain.TermsAcceptedAuditEntry(tenantID, uuid.New(), *terms, domain.SignupTypeTrial, "pro", "")
	key := "TermsAccepted:" + tenantID.String() + ":x:2026-09"
	require.NoError(t, repo.EnqueueOnce(withTenant(ctx, tenantID), key, entry))
	require.NoError(t, repo.EnqueueOnce(withTenant(ctx, tenantID), key, entry), "a repeated key is a no-op")
	require.Error(t, repo.EnqueueOnce(withTenant(ctx, tenantID), "", entry))

	claimed, err := sysRepo.ClaimDue(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, key, claimed[0].IdempotencyKey)
	assert.Equal(t, key, claimed[0].Key(), "the drainer sends the deterministic key as Idempotency-Key")
	assert.Equal(t, "203.0.113.7", claimed[0].Entry.IPAddress)

	// Delivered → the row (and its IP/user agent) is gone from this service.
	require.NoError(t, sysRepo.MarkDelivered(ctx, claimed[0].ID))
	var n int
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1`, tenantID).Scan(&n))
	assert.Zero(t, n)
}
