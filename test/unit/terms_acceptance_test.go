package unit_test

// Signup BFF open gap A-4 (LLD v1.41): RP-1/RP-2 require terms_acceptance
// and enqueue the owner's TermsAccepted audit entry (legal.terms.accepted)
// in the tenant_realms transaction — never on the bus.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

func TestTermsAcceptance_Validate(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ok := func() *domain.TermsAcceptance {
		return &domain.TermsAcceptance{Version: "2026-09", AcceptedAt: now.Add(-time.Minute), IPAddress: "203.0.113.7", UserAgent: "Mozilla/5.0"}
	}
	mut := func(f func(*domain.TermsAcceptance)) *domain.TermsAcceptance { t := ok(); f(t); return t }
	cases := map[string]struct {
		terms *domain.TermsAcceptance
		valid bool
	}{
		"valid":                {ok(), true},
		"valid ipv6":           {mut(func(t *domain.TermsAcceptance) { t.IPAddress = "2001:db8::1" }), true},
		"valid 32-char ver":    {mut(func(t *domain.TermsAcceptance) { t.Version = strings.Repeat("v", 32) }), true},
		"valid dotted ver":     {mut(func(t *domain.TermsAcceptance) { t.Version = "v3.1_rc-2" }), true},
		"just under 24h":       {mut(func(t *domain.TermsAcceptance) { t.AcceptedAt = now.Add(-24*time.Hour + time.Second) }), true},
		"9m in the future":     {mut(func(t *domain.TermsAcceptance) { t.AcceptedAt = now.Add(9 * time.Minute) }), true},
		"512-byte UA":          {mut(func(t *domain.TermsAcceptance) { t.UserAgent = strings.Repeat("a", 512) }), true},
		"missing":              {nil, false},
		"empty version":        {mut(func(t *domain.TermsAcceptance) { t.Version = "" }), false},
		"33-char version":      {mut(func(t *domain.TermsAcceptance) { t.Version = strings.Repeat("v", 33) }), false},
		"version with colon":   {mut(func(t *domain.TermsAcceptance) { t.Version = "2026:09" }), false},
		"version with space":   {mut(func(t *domain.TermsAcceptance) { t.Version = "2026 09" }), false},
		"zero accepted_at":     {mut(func(t *domain.TermsAcceptance) { t.AcceptedAt = time.Time{} }), false},
		"older than 24h":       {mut(func(t *domain.TermsAcceptance) { t.AcceptedAt = now.Add(-25 * time.Hour) }), false},
		"11m in the future":    {mut(func(t *domain.TermsAcceptance) { t.AcceptedAt = now.Add(11 * time.Minute) }), false},
		"bad ip":               {mut(func(t *domain.TermsAcceptance) { t.IPAddress = "999.1.1.1" }), false},
		"empty ip":             {mut(func(t *domain.TermsAcceptance) { t.IPAddress = "" }), false},
		"empty UA":             {mut(func(t *domain.TermsAcceptance) { t.UserAgent = " " }), false},
		"513-byte UA":          {mut(func(t *domain.TermsAcceptance) { t.UserAgent = strings.Repeat("a", 513) }), false},
		"UA with newline":      {mut(func(t *domain.TermsAcceptance) { t.UserAgent = "Mozilla\nX-Injected: 1" }), false},
		"UA with NUL":          {mut(func(t *domain.TermsAcceptance) { t.UserAgent = "Mozilla\x00" }), false},
		"UA with invalid UTF8": {mut(func(t *domain.TermsAcceptance) { t.UserAgent = "Mozilla\xff" }), false},
	}
	for name, tc := range cases {
		err := tc.terms.Validate(now)
		if tc.valid {
			assert.NoError(t, err, name)
			continue
		}
		var de *domain.Error
		require.True(t, errors.As(err, &de), "%s: %v", name, err)
		assert.Equal(t, domain.ErrInvalidRequest, de.Code, name)
		assert.Equal(t, 400, de.Status(), name)
	}
}

func TestTermsAcceptedIdempotencyKey_Deterministic(t *testing.T) {
	tenant, user := uuid.New(), uuid.New()
	assert.Equal(t, "TermsAccepted:"+tenant.String()+":"+user.String()+":2026-09", domain.TermsAcceptedIdempotencyKey(tenant, user, "2026-09"))
	assert.NotEqual(t, domain.TermsAcceptedIdempotencyKey(tenant, user, "2026-09"), domain.TermsAcceptedIdempotencyKey(tenant, user, "2027-01"),
		"a later terms version is its own record")
}

type termsTestDeps struct {
	svc   *service.ProvisioningService
	kc    *mockKeycloakAdmin
	idem  *mockIdempotencyStore
	pub   *mockEventPublisher
	audit *mockPendingAuditEntryRepository
	owner uuid.UUID
}

func newTermsTestService(t *testing.T) *termsTestDeps {
	t.Helper()
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	idem := newMockIdempotencyStore()
	tx, pub := newMockTxRunner()
	audit := &mockPendingAuditEntryRepository{}
	d := &termsTestDeps{
		svc: service.NewProvisioningService(newMockTenantRealmRepository(), idem, kc, ts, tx, audit, "trial", nil),
		kc:  kc, idem: idem, pub: pub, audit: audit, owner: uuid.New(),
	}
	kc.CreateTrialUserFn = func(context.Context, port.CreateUserRequest) (uuid.UUID, error) { return d.owner, nil }
	kc.CreateRealmFromTemplateFn = func(context.Context, string, string) error { return nil }
	kc.RegisterOIDCClientsFn = func(context.Context, string, string) ([]string, error) { return []string{"tender-web"}, nil }
	kc.CreateInvitedUserFn = func(context.Context, string, port.CreateUserRequest) (uuid.UUID, error) { return d.owner, nil }
	return d
}

func trialRequest(tenantID uuid.UUID, terms *domain.TermsAcceptance) service.ProvisionTrialRequest {
	return service.ProvisionTrialRequest{
		TenantID: tenantID, TenantName: "Acme", TenantSlug: "acme",
		AdminEmail: "owner@acme.example", AdminName: "Dana Owner", Plan: "pro", Terms: terms,
	}
}

// assertNoTermsOnBus: IP address and user agent never ride an event.
func assertNoTermsOnBus(t *testing.T, events []*domain.Event, terms *domain.TermsAcceptance) {
	t.Helper()
	for _, ev := range events {
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		assert.NotContains(t, string(b), terms.IPAddress, "%s carries the IP address", ev.Type)
		assert.NotContains(t, string(b), terms.UserAgent, "%s carries the user agent", ev.Type)
	}
}

func TestProvisionTrial_EnqueuesTermsAccepted(t *testing.T) {
	d := newTermsTestService(t)
	tenantID := uuid.New()
	terms := validTerms()

	_, err := d.svc.ProvisionTrial(context.Background(), "key-terms-1", trialRequest(tenantID, terms))
	require.NoError(t, err)

	require.Len(t, d.audit.Enqueued, 1)
	assert.Equal(t, []string{"TermsAccepted:" + tenantID.String() + ":" + d.owner.String() + ":2026-09"}, d.audit.EnqueuedKeys)
	e := d.audit.Enqueued[0]
	assert.Equal(t, tenantID, e.TenantID)
	assert.Equal(t, "legal.terms.accepted", e.EntryType)
	assert.Equal(t, "accept", e.Action)
	assert.Equal(t, domain.AuditActor{Type: "user", ID: d.owner.String()}, e.Actor)
	assert.Equal(t, &domain.AuditTarget{Type: "tenant", ID: tenantID.String()}, e.Target)
	assert.True(t, terms.AcceptedAt.Equal(e.OccurredAt), "occurred_at is the acceptance time")
	assert.Equal(t, "iam-realm-provisioner", e.SourceService)
	assert.Equal(t, "TermsAccepted", e.SourceEventType)
	assert.Equal(t, terms.IPAddress, e.IPAddress)
	assert.Equal(t, terms.UserAgent, e.UserAgent)
	assert.JSONEq(t, `{"terms_version":"2026-09","accepted_at":"`+terms.AcceptedAt.UTC().Format(time.RFC3339Nano)+`","signup_type":"trial","plan":"pro"}`, string(e.Metadata))

	require.Len(t, d.pub.events, 1)
	assertNoTermsOnBus(t, d.pub.events, terms)
}

func TestProvisionDirect_EnqueuesTermsAccepted(t *testing.T) {
	d := newTermsTestService(t)
	tenantID := uuid.New()
	terms := validTerms()

	_, err := d.svc.ProvisionDirect(context.Background(), "pd-terms-1", service.ProvisionDirectRequest{
		TenantID: tenantID, TenantName: "Acme", TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A",
		Plan: "enterprise", Terms: terms,
	})
	require.NoError(t, err)

	require.Len(t, d.audit.Enqueued, 1)
	e := d.audit.Enqueued[0]
	assert.Equal(t, domain.AuditActor{Type: "user", ID: d.owner.String()}, e.Actor)
	assert.JSONEq(t, `{"terms_version":"2026-09","accepted_at":"`+terms.AcceptedAt.UTC().Format(time.RFC3339Nano)+`","signup_type":"direct_paid","plan":"enterprise"}`, string(e.Metadata))
	require.Len(t, d.pub.events, 4)
	assertNoTermsOnBus(t, d.pub.events, terms)
}

// A replay of a completed key is answered from the ledger: no second
// entry, and no 400 even though accepted_at has since aged past 24h.
func TestProvisionTrial_ReplayDoesNotDuplicateTermsAccepted(t *testing.T) {
	d := newTermsTestService(t)
	tenantID := uuid.New()
	first, err := d.svc.ProvisionTrial(context.Background(), "key-terms-replay", trialRequest(tenantID, validTerms()))
	require.NoError(t, err)

	stale := validTerms()
	stale.AcceptedAt = time.Now().Add(-48 * time.Hour)
	second, err := d.svc.ProvisionTrial(context.Background(), "key-terms-replay", trialRequest(tenantID, stale))
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Len(t, d.audit.Enqueued, 1)
}

// The user-exists conflict resume path (or a retry under a new
// Idempotency-Key) resolves to the same owner, so the deterministic key
// dedupes — even with a different accepted_at for the same version.
func TestProvisionTrial_ResumedSignupDoesNotDuplicateTermsAccepted(t *testing.T) {
	d := newTermsTestService(t)
	tenantID := uuid.New()
	_, err := d.svc.ProvisionTrial(context.Background(), "key-terms-a", trialRequest(tenantID, validTerms()))
	require.NoError(t, err)

	d.kc.CreateTrialUserFn = func(context.Context, port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.Nil, domain.NewError(domain.ErrUserEmailConflict, "exists")
	}
	d.kc.FindUserByEmailFn = func(context.Context, string, string) (*port.ExistingUser, error) {
		return &port.ExistingUser{ID: d.owner, TenantID: tenantID}, nil
	}
	again := validTerms()
	again.AcceptedAt = again.AcceptedAt.Add(30 * time.Second)
	_, err = d.svc.ProvisionTrial(context.Background(), "key-terms-b", trialRequest(tenantID, again))
	require.NoError(t, err)

	assert.Len(t, d.audit.EnqueuedKeys, 1, "one record per owner per terms version")
}

func TestProvisionTrial_InvalidTerms_Rejected400BeforeKeycloak(t *testing.T) {
	stale := validTerms()
	stale.AcceptedAt = time.Now().Add(-25 * time.Hour)
	future := validTerms()
	future.AcceptedAt = time.Now().Add(time.Hour)
	for name, terms := range map[string]*domain.TermsAcceptance{"missing": nil, "stale": stale, "future": future} {
		d := newTermsTestService(t)
		d.kc.CreateTrialUserFn = nil // a call would panic
		key := "key-invalid-" + name
		_, err := d.svc.ProvisionTrial(context.Background(), key, trialRequest(uuid.New(), terms))
		var de *domain.Error
		require.True(t, errors.As(err, &de), "%s: %v", name, err)
		assert.Equal(t, domain.ErrInvalidRequest, de.Code, name)
		assert.Equal(t, domain.OpStatusFailed, d.idem.rows[key].Status, "%s: the key is freed for a corrected retry", name)
		assert.Empty(t, d.audit.Enqueued, name)
	}
}

func TestProvisionDirect_InvalidTerms_Rejected400(t *testing.T) {
	d := newTermsTestService(t)
	_, err := d.svc.ProvisionDirect(context.Background(), "pd-terms-missing", service.ProvisionDirectRequest{
		TenantID: uuid.New(), TenantSlug: "acme", AdminEmail: "a@acme.example", AdminName: "A", Plan: "pro",
	})
	var de *domain.Error
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
}

// The entry shares the tenant_realms transaction: an enqueue failure fails
// the signup commit (no event, key failed) rather than losing the record.
func TestProvisionTrial_TermsEnqueueFailure_FailsTheCommit(t *testing.T) {
	d := newTermsTestService(t)
	d.audit.EnqueueErr = errors.New("insert pending_audit_entries failed")
	_, err := d.svc.ProvisionTrial(context.Background(), "key-terms-enqueue-fail", trialRequest(uuid.New(), validTerms()))
	require.ErrorIs(t, err, d.audit.EnqueueErr)
	assert.Empty(t, d.pub.events)
	assert.Equal(t, domain.OpStatusFailed, d.idem.rows["key-terms-enqueue-fail"].Status)
}

// Without a pending_audit_entries repository the signup refuses to commit
// rather than silently skipping the legal record.
func TestProvisionTrial_NoAuditRepository_FailsClosed(t *testing.T) {
	kc := &mockKeycloakAdmin{}
	ts := wirePlatformAutomationHappyPath(kc)
	kc.CreateTrialUserFn = func(context.Context, port.CreateUserRequest) (uuid.UUID, error) { return uuid.New(), nil }
	tx, pub := newMockTxRunner()
	svc := service.NewProvisioningService(newMockTenantRealmRepository(), newMockIdempotencyStore(), kc, ts, tx, nil, "trial", nil)
	_, err := svc.ProvisionTrial(context.Background(), "key-no-audit", trialRequest(uuid.New(), validTerms()))
	require.Error(t, err)
	assert.Empty(t, pub.events)
}
