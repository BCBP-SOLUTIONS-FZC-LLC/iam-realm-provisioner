package eventbus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
)

func TestNewValidatingCodec_CompilesAllEmbeddedSchemas(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)
	assert.Len(t, c.schemas, len(eventschema.ByEventType))
}

func TestValidatingCodec_Encode_ValidPayloadPasses(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111","realm":"trial","keycloak_user_id":"22222222-2222-2222-2222-222222222222"}`)
	out, _, err := c.Encode(context.Background(), "TrialTenantProvisioned", payload)
	require.NoError(t, err)
	assert.Equal(t, payload, out)
}

func TestValidatingCodec_Encode_MissingRequiredFieldFailsClosed(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	// keycloak_user_id is required and missing.
	payload := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111","realm":"trial"}`)
	_, _, err = c.Encode(context.Background(), "TrialTenantProvisioned", payload)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TrialTenantProvisioned")
}

func TestValidatingCodec_Encode_WrongConstValueFailsClosed(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	// realm must be the const "trial" for this event type.
	payload := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111","realm":"dedicated","keycloak_user_id":"22222222-2222-2222-2222-222222222222"}`)
	_, _, err = c.Encode(context.Background(), "TrialTenantProvisioned", payload)
	require.Error(t, err)
}

func TestValidatingCodec_Encode_MalformedJSONFailsClosed(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	_, _, err = c.Encode(context.Background(), "TrialTenantProvisioned", []byte(`not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not JSON")
}

// TestValidatingCodec_Encode_UnregisteredEventTypePassesThrough confirms a
// consumed-only event type (no entry in eventschema.ByEventType, since that
// map holds only this service's 9 produced events) is treated as a no-op
// pass-through rather than rejected.
func TestValidatingCodec_Encode_UnregisteredEventTypePassesThrough(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"anything":"goes"}`)
	out, _, err := c.Encode(context.Background(), "TenantRoleGranted", payload)
	require.NoError(t, err)
	assert.Equal(t, payload, out)
}
