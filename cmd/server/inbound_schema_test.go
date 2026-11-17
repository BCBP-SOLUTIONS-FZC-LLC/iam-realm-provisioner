package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
)

// consumedValidator is cmd/server's adapter from the embedded consumed
// schemas to sqspipeline.Validator; the pipeline itself is tested in
// internal/adapter/inbound/sqspipeline.

func TestConsumedValidator_EveryConsumerCompiles(t *testing.T) {
	for consumer := range eventschema.ConsumedByConsumer {
		_, err := consumedValidator(consumer)
		require.NoError(t, err, consumer)
	}
}

func TestConsumedValidator_ValidAndViolatingPayloads(t *testing.T) {
	v, err := consumedValidator("tenant")
	require.NoError(t, err)
	assert.NoError(t, v.Validate("TrialStarted", []byte(`{"tenant_id":"`+uuid.NewString()+`","plan":"pro","trial_ends_at":"2026-10-07T00:00:00Z"}`)))
	assert.Error(t, v.Validate("TrialStarted", []byte(`{"tenant_id":"`+uuid.NewString()+`","plan":"pro","trial_ends_at":123}`)))
}

// An event type with no consumed schema passes (ErrNoSchema → nil), so the
// consumer's ackUnknown forward-compat path still sees it.
func TestConsumedValidator_UnknownType_Passes(t *testing.T) {
	v, err := consumedValidator("tenant")
	require.NoError(t, err)
	assert.NoError(t, v.Validate("TenantCreated", []byte(`{"anything":true}`)))
}

// Per consumer, never the produced set: Billing's TenantReactivated
// (RP-D13) validates against billing's consumed schema.
func TestConsumedValidator_BillingTenantReactivated_UsesConsumedSchema(t *testing.T) {
	v, err := consumedValidator("billing")
	require.NoError(t, err)
	assert.NoError(t, v.Validate("TenantReactivated", []byte(`{"tenant_id":"`+uuid.NewString()+`","source":"billing"}`)))
}

// A consumer name that is not in eventschema.ConsumedByConsumer yields a nil
// schema map. NewConsumedValidator accepts a nil/empty map (the loop is a
// no-op) so consumedValidator succeeds and returns a validator that treats
// every event type as ErrNoSchema — the same forward-compat pass-through path
// as an unknown event type on a real consumer.
func TestConsumedValidator_UnknownConsumer_ReturnsNoError(t *testing.T) {
	v, err := consumedValidator("does-not-exist")
	require.NoError(t, err, "an unknown consumer name must not cause an error — it produces an empty-schema validator")
	require.NotNil(t, v)
	// Every event type is treated as ErrNoSchema → passes silently.
	assert.NoError(t, v.Validate("AnyEventType", []byte(`{"anything":true}`)))
}
