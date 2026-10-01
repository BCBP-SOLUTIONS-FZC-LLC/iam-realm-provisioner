package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// These tests run against the REAL embedded consumed-message schemas
// (eventschema.ConsumedByConsumer), so they pin the actual contracts.

type recordingHandler struct {
	calls int
	err   error
}

func (r *recordingHandler) handle(context.Context, events.Envelope[json.RawMessage]) error {
	r.calls++
	return r.err
}

func realConsumedValidator(t *testing.T, consumer string) payloadValidator {
	t.Helper()
	v, err := consumedValidator(consumer)
	require.NoError(t, err)
	return v
}

func envOf(eventType, payload string) events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{ID: uuid.NewString(), Type: eventType, Source: "test", Payload: json.RawMessage(payload)}
}

func TestConsumedValidator_EveryConsumerCompiles(t *testing.T) {
	for consumer := range eventschema.ConsumedByConsumer {
		_, err := consumedValidator(consumer)
		require.NoError(t, err, consumer)
	}
}

func TestValidateConsumed_ValidPayloads_ReachHandler(t *testing.T) {
	cases := []struct{ consumer, eventType, payload string }{
		{"membership", "DepartmentMembershipGranted", `{"user_id":"` + uuid.NewString() + `","tenant_id":"` + uuid.NewString() + `","department_id":"` + uuid.NewString() + `","level":"approver","actor_id":"` + uuid.NewString() + `"}`},
		{"membership", "TenantRoleRevoked", `{"user_id":"` + uuid.NewString() + `","tenant_id":"` + uuid.NewString() + `","role_code":"tenant_admin","actor_id":"` + uuid.NewString() + `","added_later":true}`},
		{"tenant", "TrialStarted", `{"tenant_id":"` + uuid.NewString() + `","plan":"pro","trial_ends_at":"2026-10-07T00:00:00Z"}`},
		{"billing", "TenantReactivated", `{"tenant_id":"` + uuid.NewString() + `","source":"billing"}`},
	}
	for _, tc := range cases {
		t.Run(tc.consumer+"/"+tc.eventType, func(t *testing.T) {
			inner := &recordingHandler{}
			h := validateConsumed(tc.consumer, inner.handle, realConsumedValidator(t, tc.consumer), nil)
			require.NoError(t, h(context.Background(), envOf(tc.eventType, tc.payload)))
			assert.Equal(t, 1, inner.calls)
		})
	}
}

func TestValidateConsumed_Violations_RejectedBeforeHandler(t *testing.T) {
	cases := map[string]struct{ consumer, eventType, payload string }{
		"missing required field": {"membership", "TenantRoleGranted", `{"user_id":"` + uuid.NewString() + `","tenant_id":"` + uuid.NewString() + `","role_code":"tenant_admin"}`},
		"enum violation":         {"membership", "DepartmentMembershipLevelChanged", `{"user_id":"` + uuid.NewString() + `","tenant_id":"` + uuid.NewString() + `","department_id":"` + uuid.NewString() + `","previous_level":"reviewer","new_level":"boss","actor_id":"` + uuid.NewString() + `"}`},
		"wrong type":             {"tenant", "TrialStarted", `{"tenant_id":"` + uuid.NewString() + `","plan":"pro","trial_ends_at":123}`},
		"billing tenant missing": {"billing", "TenantReactivated", `{"source":"billing"}`},
		"not JSON":               {"tenant", "TrialStarted", `not-json`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ensureMetricsRegistered(t)
			before := counterValue(t, metrics.ConsumedSchemaViolationsTotal.WithLabelValues(tc.consumer, tc.eventType))
			inner := &recordingHandler{}
			log := &fakeLogger{}
			h := validateConsumed(tc.consumer, inner.handle, realConsumedValidator(t, tc.consumer), log)

			err := h(context.Background(), envOf(tc.eventType, tc.payload))
			assert.ErrorIs(t, err, errSchemaViolation)
			assert.Zero(t, inner.calls, "a violating payload must never reach the handler")
			assert.Equal(t, int32(1), log.warnCalls)
			assert.Equal(t, before+1, counterValue(t, metrics.ConsumedSchemaViolationsTotal.WithLabelValues(tc.consumer, tc.eventType)))
		})
	}
}

// TestValidateConsumed_BillingTenantReactivated_NotValidatedAsProduced pins
// the RP-D13 collision: Billing's TenantReactivated must be checked against
// the consumed billing schema, never this service's own produced
// TenantReactivated (which requires realm and source=operator and would
// reject every real Billing event).
func TestValidateConsumed_BillingTenantReactivated_NotValidatedAsProduced(t *testing.T) {
	billingPayload := `{"tenant_id":"` + uuid.NewString() + `","source":"billing"}`

	inner := &recordingHandler{}
	h := validateConsumed("billing", inner.handle, realConsumedValidator(t, "billing"), nil)
	require.NoError(t, h(context.Background(), envOf("TenantReactivated", billingPayload)))
	assert.Equal(t, 1, inner.calls)

	produced, err := eventbusadapter.NewConsumedValidator(eventschema.ByEventType)
	require.NoError(t, err)
	assert.Error(t, produced.Validate("TenantReactivated", []byte(billingPayload)),
		"precondition: the produced schema would reject Billing's payload")
}

// TestValidateConsumed_BillingSourceNotEnforced: Billing owns this schema
// and none is available to verify against, so source is deliberately not
// constrained — a different value must not drop a reactivation.
func TestValidateConsumed_BillingSourceNotEnforced(t *testing.T) {
	inner := &recordingHandler{}
	h := validateConsumed("billing", inner.handle, realConsumedValidator(t, "billing"), nil)
	require.NoError(t, h(context.Background(), envOf("TenantReactivated", `{"tenant_id":"`+uuid.NewString()+`","source":"billing-service"}`)))
	require.NoError(t, h(context.Background(), envOf("TenantReactivated", `{"tenant_id":"`+uuid.NewString()+`"}`)))
	assert.Equal(t, 2, inner.calls)
}

func TestValidateConsumed_UnknownType_PassesThroughForAckUnknown(t *testing.T) {
	inner := &recordingHandler{}
	h := validateConsumed("tenant", inner.handle, realConsumedValidator(t, "tenant"), nil)
	require.NoError(t, h(context.Background(), envOf("TenantCreated", `{"anything":true}`)))
	assert.Equal(t, 1, inner.calls)
}

func TestValidateConsumed_HandlerErrorPropagatesUnchanged(t *testing.T) {
	transient := errors.New("keycloak down")
	inner := &recordingHandler{err: transient}
	h := validateConsumed("tenant", inner.handle, realConsumedValidator(t, "tenant"), nil)
	err := h(context.Background(), envOf("TrialStarted", `{"tenant_id":"`+uuid.NewString()+`","plan":"starter","trial_ends_at":"2026-10-07T00:00:00Z"}`))
	assert.Same(t, transient, err)
}

// TestInboundHandler_SchemaViolation_ReturnsError exercises the composed
// pipeline: the violation is returned (so SQS retries until redrive, and
// platform-events' consumer counts the delivery in
// platform_messages_failed_total) and the inner handler never runs.
func TestInboundHandler_SchemaViolation_ReturnsError(t *testing.T) {
	ensureMetricsRegistered(t)

	inner := &recordingHandler{}
	h := inboundHandler("billing", inner.handle, realConsumedValidator(t, "billing"), &fakeLogger{})
	err := h(context.Background(), envOf("TenantReactivated", `{"source":"billing"}`))

	assert.ErrorIs(t, err, errSchemaViolation)
	assert.Zero(t, inner.calls)
}
