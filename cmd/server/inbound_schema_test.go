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
