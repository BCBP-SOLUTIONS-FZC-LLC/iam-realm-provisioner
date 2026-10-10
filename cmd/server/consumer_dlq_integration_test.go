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

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// TestConsumerOptions_DLQHandlerObservesLastDeliveryBeforeRedrive runs this
// service's real consumer pipeline (consumerOptions + inboundHandler) against
// a floci SQS queue whose RedrivePolicy matches production
// (maxReceiveCount=5). A handler that always fails must see deliveries 1-4;
// dlqHandler must take the 5th (platform_dlq_messages_total +1, message left
// visible); SQS itself then redrives the message to -dlq.
//
// Queues are driven over floci's SQS JSON protocol with plain HTTP because
// arch-lint's events invariant forbids importing the raw SQS SDK outside
// cmd/server/main.go (test files included); the consumer builds its own
// client via events.NewSQSConsumer.
func TestConsumerOptions_DLQHandlerObservesLastDeliveryBeforeRedrive(t *testing.T) {
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

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	dlqURL := sqsCall(t, endpoint, "CreateQueue", map[string]any{"QueueName": "tenant-realm-q-dlq-" + suffix})["QueueUrl"].(string)
	dlqArn := sqsCall(t, endpoint, "GetQueueAttributes", map[string]any{"QueueUrl": dlqURL, "AttributeNames": []string{"QueueArn"}})["Attributes"].(map[string]any)["QueueArn"].(string)
	queueURL := sqsCall(t, endpoint, "CreateQueue", map[string]any{
		"QueueName":  "tenant-realm-q-" + suffix,
		"Attributes": map[string]string{"RedrivePolicy": `{"deadLetterTargetArn":"` + dlqArn + `","maxReceiveCount":"5"}`},
	})["QueueUrl"].(string)

	body, err := json.Marshal(map[string]any{
		"id": uuid.NewString(), "type": "TrialStarted", "source": "iam-org-membership", "specversion": "1",
		"tenant_id": uuid.NewString(), "time": time.Now().UTC(),
		"data": map[string]any{"tenant_id": uuid.NewString(), "plan": "pro", "trial_ends_at": "2026-10-07T00:00:00Z"},
	})
	require.NoError(t, err)
	sqsCall(t, endpoint, "SendMessage", map[string]any{"QueueUrl": queueURL, "MessageBody": string(body)})

	// platform-events v2 records platform_dlq_messages_total internally via
	// WithDeadLetterHandler; this service no longer maintains its own DLQ
	// counter. The test verifies behavior via handler-call count + queue depth.
	var handlerCalls int32
	failing := func(context.Context, events.Envelope[json.RawMessage]) error {
		atomic.AddInt32(&handlerCalls, 1)
		return fmt.Errorf("transient failure")
	}
	validator, err := consumedValidator("tenant")
	require.NoError(t, err)

	cons, err := events.NewSQSConsumer(
		events.SQSConfig{QueueURL: queueURL, Region: "ap-south-1", EndpointURL: endpoint, MaxMessages: 1, WaitSeconds: 1},
		inboundHandler("tenant", failing, validator, &fakeLogger{}),
		append(consumerOptions("tenant", 1), events.WithVisibilityTimeout(time.Second))...,
	)
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = cons.Start(runCtx) }()
	defer func() { _ = cons.Stop() }()

	// Wait until the handler has been called dlqObserveReceiveCount times —
	// the dlqHandler fires on the (dlqObserveReceiveCount+1)-th delivery,
	// leaving the message visible so SQS redrives it to -dlq.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&handlerCalls) >= dlqObserveReceiveCount
	}, 60*time.Second, 200*time.Millisecond, "handler never reached dlqObserveReceiveCount deliveries")
	assert.EqualValues(t, dlqObserveReceiveCount, atomic.LoadInt32(&handlerCalls),
		"the handler sees deliveries 1-4; dlqHandler takes the 5th")

	require.Eventually(t, func() bool {
		attrs := sqsCall(t, endpoint, "GetQueueAttributes", map[string]any{"QueueUrl": dlqURL, "AttributeNames": []string{"ApproximateNumberOfMessages"}})["Attributes"].(map[string]any)
		return attrs["ApproximateNumberOfMessages"] == "1"
	}, 60*time.Second, 500*time.Millisecond, "SQS never redrove the message to -dlq")
	assert.EqualValues(t, dlqObserveReceiveCount, atomic.LoadInt32(&handlerCalls), "no handler call after the observer")
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
