//go:build integration

// This file exercises the OpenBao-backed port.OpenBaoClient adapter
// (internal/adapter/outbound/openbao/client.go) against a REAL OpenBao
// testcontainer — the real-KV-v2-wire-behavior counterpart to
// client_test.go's httptest-fake-backed unit suite. See
// openbao_helpers_test.go's setupOpenBao doc comment for exactly how
// Kubernetes-auth is exercised for real without a live Kubernetes cluster,
// and why (openbao/openbao:latest's kubernetes-auth plugin does not honour
// pem_keys as a TokenReview-skip the way older Vault docs describe).
//
// Requires Docker on the runner. Tag: integration.
package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

func TestOpenBao_WriteIdpSecret_ReadBack_RoundTrip(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env, baoIdpPrefix)
	ctx := context.Background()

	tenantID := uuid.New()
	secret := map[string]any{"client_secret": "s3cr3t-value", "cert": "abc123"}

	require.NoError(t, client.WriteIdpSecret(ctx, tenantID, "okta-saml", secret))

	got, err := client.ReadIdpSecret(ctx, tenantID, "okta-saml")
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t-value", got["client_secret"])
	assert.Equal(t, "abc123", got["cert"])

	// Independent check, bypassing the adapter entirely: read the exact KV
	// v2 path with the root token and confirm the secret really is there,
	// at exactly RP-D10's deterministic path shape.
	wantPath := fmt.Sprintf("/v1/secret/data/%s/%s/okta-saml", baoIdpPrefix, tenantID)
	var raw struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	baoDo(t, "GET", env.addr, env.rootToken, wantPath, nil, &raw)
	assert.Equal(t, "s3cr3t-value", raw.Data.Data["client_secret"])
	assert.Equal(t, "abc123", raw.Data.Data["cert"])
}

func TestOpenBao_ReadIdpSecret_NeverWritten_ReturnsDomainNotFound(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env, baoIdpPrefix)
	ctx := context.Background()

	// Independent check of the real not-found response shape before going
	// through the adapter — confirms client_test.go's fakeBao assumption
	// (`{"errors":[]}` on a 404) matches the real server (see final report
	// for how this was verified; no discrepancy found).
	tenantID := uuid.New()
	rawPath := fmt.Sprintf("/v1/secret/data/%s/%s/never-written", baoIdpPrefix, tenantID)
	resp := baoDoRaw(t, "GET", env.addr, env.rootToken, rawPath, nil)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, 404, resp.StatusCode)

	_, err := client.ReadIdpSecret(ctx, tenantID, "never-written")
	require.Error(t, err)
	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	assert.Equal(t, domain.ErrIdpConfigNotFound, domErr.Code)
	assert.Equal(t, 404, domErr.Status())
}

func TestOpenBao_DeleteIdpSecret_RemovesReal_AndIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env, baoIdpPrefix)
	ctx := context.Background()

	tenantID := uuid.New()
	require.NoError(t, client.WriteIdpSecret(ctx, tenantID, "azure-oidc", map[string]any{"x": "y"}))

	// Confirm it's really there via an independent root-token read before
	// deleting.
	rawPath := fmt.Sprintf("/v1/secret/data/%s/%s/azure-oidc", baoIdpPrefix, tenantID)
	preResp := baoDoRaw(t, "GET", env.addr, env.rootToken, rawPath, nil)
	_ = preResp.Body.Close()
	require.Equal(t, 200, preResp.StatusCode)

	require.NoError(t, client.DeleteIdpSecret(ctx, tenantID, "azure-oidc"))

	// Independent check: gone for real, not just from the adapter's point
	// of view.
	postResp := baoDoRaw(t, "GET", env.addr, env.rootToken, rawPath, nil)
	_ = postResp.Body.Close()
	assert.Equal(t, 404, postResp.StatusCode)

	// KV v2's metadata-delete is a full delete of every version (RP-7 wants
	// the secret permanently gone, not just soft-deleted/undeletable) — a
	// second delete of the now-absent path must still succeed (idempotent
	// remove, RP-7's contract).
	require.NoError(t, client.DeleteIdpSecret(ctx, tenantID, "azure-oidc"))

	// And deleting a path that was never written at all is equally a no-op
	// success.
	require.NoError(t, client.DeleteIdpSecret(ctx, uuid.New(), "never-existed"))
}

// TestOpenBao_PermissionDenied_SurfacesRealError confirms the adapter
// surfaces a real OpenBao permission-denied error correctly, using the
// SAME real Kubernetes-auth role/token this suite's other tests use (so
// login itself succeeds) but a Client configured with an IdpPathPrefix
// outside deploy/openbao/policy.hcl's granted paths — the deterministic
// idpPath construction (RP-D10) means the *adapter* itself never
// constructs an out-of-policy path in production; this is the only way to
// reach one from a Client that is still doing a real login+KV round trip.
func TestOpenBao_PermissionDenied_SurfacesRealError(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	outOfPolicyClient := newClient(env, "not-allowed/other-scope")
	ctx := context.Background()

	err := outOfPolicyClient.WriteIdpSecret(ctx, uuid.New(), "okta-saml", map[string]any{"x": "y"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write_secret")
	assert.Contains(t, err.Error(), "permission denied")

	_, err = outOfPolicyClient.ReadIdpSecret(ctx, uuid.New(), "okta-saml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read_secret")
	assert.Contains(t, err.Error(), "permission denied")
}
