package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events/mock"
)

const flociQueueURL = "http://floci:4566/000000000000/tenant-realm-q"

func handlerReturning(err error) events.Handler {
	return func(context.Context, events.Envelope[json.RawMessage]) error { return err }
}

func testEnvelope() events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{
		ID: uuid.NewString(), Type: "TrialStarted", Source: "iam-org-membership",
		TenantID: uuid.NewString(), Payload: json.RawMessage(`{"plan":"pro"}`),
	}
}

// ── RequireDLQ — startup preflight: a missing RedrivePolicy must stop
// startup, a transient failure must not disable DLQ routing ──────────────

func TestRequireDLQ_Resolved(t *testing.T) {
	require.NoError(t, RequireDLQ(context.Background(), &mock.DLQPublisher{}, flociQueueURL, &fakeLogger{}))
}

func TestRequireDLQ_NoRedrivePolicy_IsFatal(t *testing.T) {
	for _, cfgErr := range []error{events.ErrDLQNotConfigured, events.ErrDLQInvalidRedrivePolicy} {
		dlq := &mock.DLQPublisher{}
		dlq.SetError(cfgErr)
		err := RequireDLQ(context.Background(), dlq, flociQueueURL, &fakeLogger{})
		require.Error(t, err)
		assert.ErrorIs(t, err, cfgErr)
	}
}

func TestRequireDLQ_TransientFailure_WarnsAndContinues(t *testing.T) {
	dlq := &mock.DLQPublisher{}
	dlq.SetError(errors.New("throttled"))
	log := &fakeLogger{}
	require.NoError(t, RequireDLQ(context.Background(), dlq, flociQueueURL, log),
		"a transient failure must not stop startup; the publisher resolves again on the first forward")
	assert.Equal(t, int32(1), log.warnCalls)
}

// ── routeRejectsToDLQ outside a consumer dispatch ────────────────────────

func TestRouteRejects_NoSourceMessage_KeepsRetrySemantics(t *testing.T) {
	dlq := &mock.DLQPublisher{}
	h := routeRejectsToDLQ(handlerReturning(errSchemaViolation), dlq, &fakeLogger{})
	assert.ErrorIs(t, h(context.Background(), testEnvelope()), errSchemaViolation)
	assert.Empty(t, dlq.Sent())
}

func TestRouteRejects_OtherOutcomes_PassThroughUntouched(t *testing.T) {
	transient := errors.New("db down")
	for name, in := range map[string]error{"success": nil, "transient": transient} {
		t.Run(name, func(t *testing.T) {
			dlq := &mock.DLQPublisher{}
			h := routeRejectsToDLQ(handlerReturning(in), dlq, &fakeLogger{})
			assert.Equal(t, in, h(context.Background(), testEnvelope()))
			assert.Empty(t, dlq.Sent())
		})
	}
}

// ── Real platform-events consumer over a fake SQS transport ──────────────

// oneShotSQS delivers one message on the first ReceiveMessage, then idles.
// It implements the platform-events SQS client interface directly (raw SDK
// types are legitimate in test files — they simulate the external queue).
type oneShotSQS struct {
	mu      sync.Mutex
	msg     *sqstypes.Message
	deleted chan struct{}
}

func (s *oneShotSQS) ReceiveMessage(ctx context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	s.mu.Lock()
	msg := s.msg
	s.msg = nil
	s.mu.Unlock()
	if msg != nil {
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{*msg}}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return &sqs.ReceiveMessageOutput{}, nil
	}
}

func (s *oneShotSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	close(s.deleted)
	return &sqs.DeleteMessageOutput{}, nil
}

func (s *oneShotSQS) ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func newOneShot(body string, receiveCount string, attrs map[string]string) *oneShotSQS {
	msg := &sqstypes.Message{
		MessageId: aws.String("m-1"), ReceiptHandle: aws.String("rh-1"), Body: aws.String(body),
		Attributes: map[string]string{"ApproximateReceiveCount": receiveCount},
	}
	if len(attrs) > 0 {
		msg.MessageAttributes = map[string]sqstypes.MessageAttributeValue{}
		for k, v := range attrs {
			msg.MessageAttributes[k] = sqstypes.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(v)}
		}
	}
	return &oneShotSQS{msg: msg, deleted: make(chan struct{})}
}

func testSQSEnv() eventcfg.SQSConfigEnv {
	return EnvForQueue(eventcfg.SQSConfigEnv{QueueURL: flociQueueURL, Region: "ap-south-1", Concurrency: 1, MaxMessages: 1, WaitSeconds: 1, VisibilityTimeout: 30 * time.Second}, flociQueueURL, 1)
}

// runConsumer starts a consumer and returns whether the message was deleted
// within wait.
func runConsumer(t *testing.T, client *oneShotSQS, handler events.Handler, dlq events.DLQPublisher, wait time.Duration) bool {
	t.Helper()
	cons, err := NewConsumer(testSQSEnv(), client, handler, dlq, &fakeLogger{})
	require.NoError(t, err)
	go func() { _ = cons.Start(t.Context()) }()
	defer func() { _ = cons.Stop() }()
	select {
	case <-client.deleted:
		return true
	case <-time.After(wait):
		return false
	}
}

// glueFramedBody is an upstream envelope whose data is Glue-framed and whose
// dataschema is set, as O&M publishes it.
func glueFramedBody(t *testing.T, eventType, payload string) string {
	t.Helper()
	versionID := uuid.New()
	framed := append([]byte{0x03, 0x00}, versionID[:]...)
	framed = append(framed, payload...)
	data, err := json.Marshal(framed) // []byte marshals as a base64 JSON string (WrapCodecPayload's format)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"id": uuid.NewString(), "type": eventType, "source": "iam-org-membership",
		"tenant_id": uuid.NewString(), "dataschema": versionID.String(),
		"time": time.Now().UTC(), "data": json.RawMessage(data),
	})
	require.NoError(t, err)
	return string(body)
}

// The consumer carries the library's decode-only Glue codec: an upstream
// envelope with a dataschema and a base64 Glue-framed payload reaches the
// handler as plain JSON and is acked.
func TestNewConsumer_DecodesGlueEncodedUpstreamPayload(t *testing.T) {
	plain := `{"tenant_id":"` + uuid.NewString() + `","plan":"pro","trial_ends_at":"2026-10-07T00:00:00Z"}`
	client := newOneShot(glueFramedBody(t, "TrialStarted", plain), "1", nil)
	got := make(chan json.RawMessage, 1)
	handler := func(_ context.Context, env events.Envelope[json.RawMessage]) error {
		got <- env.Payload
		return nil
	}
	require.True(t, runConsumer(t, client, handler, &mock.DLQPublisher{}, 5*time.Second), "message was not acked")
	select {
	case payload := <-got:
		assert.JSONEq(t, plain, string(payload))
	default:
		t.Fatal("handler never received the message — Glue decode likely failed")
	}
}

// A body that is not a valid envelope lands in the DLQ byte-for-byte
// (WithDLQForwarding) instead of being deleted and only logged.
func TestConsumer_MalformedBody_ForwardedToDLQNotDropped(t *testing.T) {
	const body = `this is not an event envelope`
	client := newOneShot(body, "1", map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	dlq := &mock.DLQPublisher{}
	handlerCalled := false
	h := func(context.Context, events.Envelope[json.RawMessage]) error { handlerCalled = true; return nil }

	require.True(t, runConsumer(t, client, h, dlq, 5*time.Second), "forwarded message must then be deleted")
	assert.False(t, handlerCalled)
	sent := dlq.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, body, string(sent[0].Body))
	assert.Equal(t, flociQueueURL, sent[0].SourceQueueURL)
	assert.Equal(t, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", sent[0].Attrs["traceparent"])
}

// Through the full inbound pipeline: a schema violation is forwarded on its
// FIRST delivery with DLQReason=schema_violation, as the original transport
// message (Glue framing intact), and never reaches the handler — it no
// longer burns four redeliveries waiting for SQS's redrive.
func TestConsumer_SchemaViolation_ForwardedOnFirstDelivery(t *testing.T) {
	body := glueFramedBody(t, "TenantReactivated", `{"source":"billing"}`)
	attrs := map[string]string{"EventType": "TenantReactivated"}
	client := newOneShot(body, "1", attrs)
	dlq := &mock.DLQPublisher{}
	inner := &recordingHandler{}

	h := Handler("billing", "billing-realm-q", inner.handle, realValidator(t, "billing"), dlq, &fakeLogger{})
	require.True(t, runConsumer(t, client, h, dlq, 5*time.Second))
	assert.Zero(t, inner.calls)
	sent := dlq.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, schemaViolationReason, sent[0].Reason)
	assert.Equal(t, body, string(sent[0].Body), "the original body, Glue framing included")
	assert.Equal(t, "TenantReactivated", sent[0].Attrs["EventType"])
}

// A still-failing message is forwarded by the library itself on the
// delivery whose receive count exceeds dlqForwardReceiveCount (the 5th) —
// the last one before the queues' RedrivePolicy maxReceiveCount=5 would
// move it, so the forward (with its reason) wins over SQS's silent redrive.
// The 4th delivery still reaches the handler.
func TestConsumer_TransientFailure_OverThreshold_ForwardedByLibrary(t *testing.T) {
	body := glueFramedBody(t, "TrialStarted", `{"tenant_id":"`+uuid.NewString()+`","plan":"pro","trial_ends_at":"2026-10-07T00:00:00Z"}`)
	client := newOneShot(body, "5", nil)
	dlq := &mock.DLQPublisher{}
	inner := &recordingHandler{err: errors.New("keycloak down")}

	require.True(t, runConsumer(t, client, inner.handle, dlq, 5*time.Second))
	assert.Zero(t, inner.calls, "the over-threshold delivery is forwarded, not handled")
	require.Len(t, dlq.Sent(), 1)
	assert.Equal(t, body, string(dlq.Sent()[0].Body))

	fourth := newOneShot(body, "4", nil)
	fourthDLQ := &mock.DLQPublisher{}
	assert.False(t, runConsumer(t, fourth, inner.handle, fourthDLQ, 300*time.Millisecond), "the 4th delivery is handled, fails, and stays on the queue")
	assert.Equal(t, 1, inner.calls)
	assert.Empty(t, fourthDLQ.Sent())
}

// A failed forward leaves the message on the queue: it is neither acked nor
// lost, so retry and SQS redrive remain the backstop.
func TestConsumer_DLQSendFails_MessageStaysOnQueue(t *testing.T) {
	body := glueFramedBody(t, "TenantReactivated", `{"source":"billing"}`)
	client := newOneShot(body, "1", nil)
	dlq := &mock.DLQPublisher{}
	log := &fakeLogger{}
	h := routeRejectsToDLQ(func(context.Context, events.Envelope[json.RawMessage]) error {
		dlq.SetError(errors.New("sqs unavailable")) // after Start's DLQ check, before the forward
		return errSchemaViolation
	}, dlq, log)

	assert.False(t, runConsumer(t, client, h, dlq, 500*time.Millisecond), "a message whose forward failed must not be deleted")
	assert.Empty(t, dlq.Sent())
	assert.GreaterOrEqual(t, log.warnCalls, int32(1))
}
