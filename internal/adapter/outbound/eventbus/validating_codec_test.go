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

// withPoisonedSchema temporarily injects (and removes) an extra entry into
// the real eventschema.ByEventType map — the only way to reach
// NewValidatingCodec's three construction-time error branches, since every
// genuine embedded schema is a fixed, always-valid asset. ByEventType is a
// plain package-level map (a reference type), so mutating it from here is
// safe as long as the entry is removed again before the test ends.
func withPoisonedSchema(t *testing.T, name string, raw []byte) {
	t.Helper()
	eventschema.ByEventType[name] = raw
	t.Cleanup(func() { delete(eventschema.ByEventType, name) })
}

func TestNewValidatingCodec_MalformedSchemaJSON_ReturnsError(t *testing.T) {
	withPoisonedSchema(t, "test-malformed-json", []byte(`not json`))
	_, err := NewValidatingCodec(NoopCodec{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse embedded schema")
}

// A map key that collides with a JSON-Schema meta-schema URI is the one
// realistic way to make compiler.AddResource itself fail (as opposed to
// Compile) — every genuine event-type name is a bare identifier, never a
// URL, so this branch is otherwise unreachable via legitimate map content.
func TestNewValidatingCodec_AddResourceCollision_ReturnsError(t *testing.T) {
	withPoisonedSchema(t, "https://json-schema.org/draft/2020-12/schema", []byte(`{"type":"object"}`))
	_, err := NewValidatingCodec(NoopCodec{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add schema resource")
}

func TestNewValidatingCodec_SchemaFailsCompilation_ReturnsError(t *testing.T) {
	withPoisonedSchema(t, "test-bad-ref", []byte(`{"$ref": "#/does/not/exist"}`))
	_, err := NewValidatingCodec(NoopCodec{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compile schema")
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
