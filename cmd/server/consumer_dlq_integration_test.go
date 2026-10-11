//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/sqspipeline"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// TestInboundPipeline_DLQForwardingAgainstFloci runs this service's real
// inbound pipeline (sqspipeline.NewConsumer + Handler, platform-events' SQS
// DLQ publisher, cmd/server's consumedValidator) against floci queues whose
// RedrivePolicy matches production (maxReceiveCount=5):
//
//  1. RequireDLQ resolves the DLQ from the queue's RedrivePolicy, and fails
//     startup for a queue that has none.
//  2. A schema-violating message goes straight to -dlq on its FIRST
//     delivery (reason schema_violation) and never reaches the handler.
//  3. A message whose handler keeps failing is forwarded to -dlq by the
//     library itself on the 5th delivery (receive count > 4), before SQS's
//     own redrive — the handler sees deliveries 1-4 only.
//
// Queues are driven over floci's SQS JSON protocol with plain HTTP because
// arch-lint's events invariant forbids the raw SQS SDK outside
// cmd/server/main.go in non-test code; the consumer and DLQ publisher build
// their own clients from events.SQSConfig / events.DLQConfig.
func TestInboundPipeline_DLQForwardingAgainstFloci(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping floci integration test in short mode")
	}
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "floci/floci:2.1.0",
			ExposedPorts: []string{"4566/tcp"},
			Env:          map[string]string{"FLOCI_DEFAULT_REGION": "ap-south-1", "FLOCI_DEFAULT_ACCOUNT_ID": "000000000000"},
			WaitingFor:   wait.ForLog("Ready.").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Skipf("skipping — floci unavailable (%v)", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "4566/tcp")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ensureMetricsRegistered(t)

	dlq, err := events.NewSQSDLQPublisher(events.DLQConfig{Region: "ap-south-1", EndpointURL: endpoint, ConsumerName: "iam-realm-provisioner", Logger: &fakeLogger{}})
	require.NoError(t, err)

	newQueuePair := func(name string) (queueURL, dlqURL string) {
		suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		dlqURL = sqsCall(t, endpoint, "CreateQueue", map[string]any{"QueueName": name + "-dlq-" + suffix})["QueueUrl"].(string)
		dlqArn := sqsCall(t, endpoint, "GetQueueAttributes", map[string]any{"QueueUrl": dlqURL, "AttributeNames": []string{"QueueArn"}})["Attributes"].(map[string]any)["QueueArn"].(string)
		queueURL = sqsCall(t, endpoint, "CreateQueue", map[string]any{
			"QueueName":  name + "-" + suffix,
			"Attributes": map[string]string{"RedrivePolicy": `{"deadLetterTargetArn":"` + dlqArn + `","maxReceiveCount":"5"}`},
		})["QueueUrl"].(string)
		return queueURL, dlqURL
	}
	newBareQueue := func(name string) string {
		return sqsCall(t, endpoint, "CreateQueue", map[string]any{"QueueName": name + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]})["QueueUrl"].(string)
	}
	dlqDepth := func(dlqURL string) string {
		attrs := sqsCall(t, endpoint, "GetQueueAttributes", map[string]any{"QueueUrl": dlqURL, "AttributeNames": []string{"ApproximateNumberOfMessages"}})["Attributes"].(map[string]any)
		return attrs["ApproximateNumberOfMessages"].(string)
	}
	send := func(queueURL, eventType string, data map[string]any) {
		body, err := json.Marshal(map[string]any{
			"id": uuid.NewString(), "type": eventType, "source": "iam-org-membership", "specversion": "1",
			"tenant_id": uuid.NewString(), "time": time.Now().UTC(), "data": data,
		})
		require.NoError(t, err)
		sqsCall(t, endpoint, "SendMessage", map[string]any{"QueueUrl": queueURL, "MessageBody": string(body)})
	}
	run := func(queueURL, consumer string, h events.Handler) {
		validator, err := consumedValidator(consumer)
		require.NoError(t, err)
		env := sqspipeline.EnvForQueue(eventcfg.SQSConfigEnv{
			Region: "ap-south-1", EndpointURL: endpoint, MaxMessages: 1, WaitSeconds: 1, VisibilityTimeout: time.Second,
		}, queueURL, 1)
		cons, err := sqspipeline.NewConsumer(env, nil, sqspipeline.Handler(consumer, consumer+"-realm-q", h, validator, dlq, &fakeLogger{}), dlq, &fakeLogger{})
		require.NoError(t, err)
		runCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		go func() { _ = cons.Start(runCtx) }()
		t.Cleanup(func() { _ = cons.Stop() })
	}

	t.Run("RequireDLQ", func(t *testing.T) {
		queueURL, _ := newQueuePair("tenant-realm-q")
		require.NoError(t, sqspipeline.RequireDLQ(ctx, dlq, queueURL, &fakeLogger{}))

		bare := newBareQueue("no-redrive")
		assert.ErrorIs(t, sqspipeline.RequireDLQ(ctx, dlq, bare, &fakeLogger{}), events.ErrDLQNotConfigured)
	})

	t.Run("SchemaViolationStraightToDLQ", func(t *testing.T) {
		queueURL, dlqURL := newQueuePair("billing-realm-q")
		var calls int32
		send(queueURL, "TenantReactivated", map[string]any{"source": "billing"}) // tenant_id missing
		run(queueURL, "billing", func(context.Context, events.Envelope[json.RawMessage]) error {
			atomic.AddInt32(&calls, 1)
			return nil
		})
		require.Eventually(t, func() bool { return dlqDepth(dlqURL) == "1" }, 30*time.Second, 200*time.Millisecond,
			"a schema violation must reach -dlq on its first delivery")
		assert.Zero(t, atomic.LoadInt32(&calls))
	})

	t.Run("PersistentFailureForwardedBeforeRedrive", func(t *testing.T) {
		queueURL, dlqURL := newQueuePair("tenant-realm-q")
		var calls int32
		send(queueURL, "TrialStarted", map[string]any{"tenant_id": uuid.NewString(), "plan": "pro", "trial_ends_at": "2026-10-07T00:00:00Z"})
		run(queueURL, "tenant", func(context.Context, events.Envelope[json.RawMessage]) error {
			atomic.AddInt32(&calls, 1)
			return fmt.Errorf("transient failure")
		})
		require.Eventually(t, func() bool { return dlqDepth(dlqURL) == "1" }, 60*time.Second, 300*time.Millisecond,
			"the library never forwarded the message")
		assert.EqualValues(t, 4, atomic.LoadInt32(&calls), "the handler sees deliveries 1-4; the 5th is forwarded")
	})
}

// sqsCall invokes one floci SQS JSON-protocol action and returns the decoded
// response body.
func sqsCall(t *testing.T, endpoint, action string, in map[string]any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(in)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS."+action)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Less(t, resp.StatusCode, 300, "%s: %v", action, out)
	return out
}
