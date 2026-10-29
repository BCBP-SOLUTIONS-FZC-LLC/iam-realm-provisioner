package unit_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
)

// Emailed links carry the workspace (HLD §8.2.7): the verify and invitation
// emails end on <web base>/t/<slug>/ via the tender-web client.

func TestProvisionTrial_VerifyEmailLinksToWorkspace(t *testing.T) {
	svc, kc, _, _, _ := newProvisioningTestService(t)
	svc.WithWebEntry("https://app.example/", "tender-web")
	var got *port.ActionLink
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		got = req.Link
		return uuid.New(), nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "ael-1", service.ProvisionTrialRequest{
		Terms: validTerms(), TenantID: uuid.New(), TenantName: "Acme", TenantSlug: "acme",
		AdminEmail: "owner@acme.example", AdminName: "Owner", Plan: "pro",
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, port.ActionLink{ClientID: "tender-web", RedirectURI: "https://app.example/t/acme/"}, *got)
}

func TestProvisionTrial_NoWebEntry_NoLink(t *testing.T) {
	svc, kc, _, _, _ := newProvisioningTestService(t)
	kc.CreateTrialUserFn = func(ctx context.Context, req port.CreateUserRequest) (uuid.UUID, error) {
		assert.Nil(t, req.Link, "without WithWebEntry the email keeps Keycloak's own page")
		return uuid.New(), nil
	}

	_, err := svc.ProvisionTrial(context.Background(), "ael-2", service.ProvisionTrialRequest{
		Terms: validTerms(), TenantID: uuid.New(), TenantName: "Acme", TenantSlug: "acme",
		AdminEmail: "owner@acme.example", AdminName: "Owner", Plan: "pro",
	})
	require.NoError(t, err)
}

func TestCreateInvitedUser_SendsInvitationEmail(t *testing.T) {
	cases := []struct {
		name     string
		row      domain.TenantRealm
		slug     string
		wantLink *port.ActionLink
	}{
		{"dedicated realm",
			domain.TenantRealm{RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}, "acme",
			&port.ActionLink{ClientID: "tender-web", RedirectURI: "https://app.example/t/acme/"}},
		{"shared realm",
			domain.TenantRealm{RealmName: "trial", Strategy: domain.RealmStrategyShared}, "beta",
			&port.ActionLink{ClientID: "tender-web", RedirectURI: "https://app.example/t/beta/"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, kc, realms, _, _ := newProvisioningTestService(t)
			svc.WithWebEntry("https://app.example", "tender-web")
			tenantID, userID := uuid.New(), uuid.New()
			row := tc.row
			row.TenantID = tenantID
			realms.rows[tenantID] = &row
			kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
				return userID, nil
			}
			sent := 0
			kc.SendActionsEmailFn = func(ctx context.Context, realmName string, id uuid.UUID, actions []string, lifespan time.Duration, link *port.ActionLink) error {
				sent++
				assert.Equal(t, tc.row.RealmName, realmName)
				assert.Equal(t, userID, id)
				assert.Equal(t, []string{"VERIFY_EMAIL", "UPDATE_PASSWORD"}, actions)
				assert.Equal(t, 7*24*time.Hour, lifespan)
				assert.Equal(t, tc.wantLink, link)
				return nil
			}

			_, err := svc.CreateInvitedUser(context.Background(), "ael-inv-"+string(rune('a'+i)), tenantID, service.CreateInvitedUserRequest{
				Email: "b@acme.example", FullName: "B", RequiredActions: []string{"VERIFY_EMAIL", "UPDATE_PASSWORD"}, TenantSlug: tc.slug,
			})
			require.NoError(t, err)
			assert.Equal(t, 1, sent)
		})
	}
}

// An invitee has no password, so a lost invitation email fails RP-5; the
// caller's retry resends it.
func TestCreateInvitedUser_InvitationEmailFailure_FailsRP5(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	kc.SendActionsEmailFn = func(ctx context.Context, realmName string, id uuid.UUID, actions []string, lifespan time.Duration, link *port.ActionLink) error {
		return domain.NewError(domain.ErrKeycloakUnavailable, "smtp down")
	}

	_, err := svc.CreateInvitedUser(context.Background(), "ael-fail", tenantID, service.CreateInvitedUserRequest{
		Email: "b@acme.example", FullName: "B", RequiredActions: []string{"VERIFY_EMAIL"}, TenantSlug: "acme",
	})
	require.Error(t, err)
	assert.Equal(t, domain.OpStatusFailed, idem.rows["ael-fail"].Status)
}

func TestCreateInvitedUser_NoRequiredActions_NoEmail(t *testing.T) {
	svc, kc, realms, _, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "tenant-acme", Strategy: domain.RealmStrategyDedicated}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		return uuid.New(), nil
	}
	kc.SendActionsEmailFn = func(ctx context.Context, realmName string, id uuid.UUID, actions []string, lifespan time.Duration, link *port.ActionLink) error {
		t.Fatal("Keycloak rejects an execute-actions email with no actions; it must not be sent")
		return nil
	}

	_, err := svc.CreateInvitedUser(context.Background(), "ael-none", tenantID, service.CreateInvitedUserRequest{Email: "b@acme.example", FullName: "B", TenantSlug: "acme"})
	require.NoError(t, err)
}

// tenant_slug is required for every invitation: without it there is no
// workspace link, so RP-5 refuses before the ledger or Keycloak is touched.
func TestCreateInvitedUser_MissingTenantSlug_InvalidRequest(t *testing.T) {
	svc, kc, realms, idem, _ := newProvisioningTestService(t)
	tenantID := uuid.New()
	realms.rows[tenantID] = &domain.TenantRealm{TenantID: tenantID, RealmName: "trial", Strategy: domain.RealmStrategyShared}
	kc.CreateInvitedUserFn = func(ctx context.Context, realmName string, req port.CreateUserRequest) (uuid.UUID, error) {
		t.Fatal("no Keycloak user without a tenant_slug")
		return uuid.Nil, nil
	}

	_, err := svc.CreateInvitedUser(context.Background(), "ael-noslug", tenantID, service.CreateInvitedUserRequest{
		Email: "b@acme.example", FullName: "B", RequiredActions: []string{"VERIFY_EMAIL"},
	})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
	assert.NotContains(t, idem.rows, "ael-noslug", "no ledger row for a rejected request")
}
