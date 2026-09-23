package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func ensureMetricsRegistered(t *testing.T) {
	t.Helper()
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-realm-provisioner", BuildVersion: "test"})
	metrics.Register("test")
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

// TestInstrumentConsumer_Success_IncrementsReceivedAndProcessed and its
// sibling below cover instrumentConsumer (wiring.go), the Enterprise
// Platform Observability Standard's Canonical Tier 1 message-lifecycle
// counters wrapping all 3 SQS consumer handlers.
func TestInstrumentConsumer_Success_IncrementsReceivedAndProcessed(t *testing.T) {
	ensureMetricsRegistered(t)
	before := counterValue(t, metrics.PlatformMessagesProcessedTotal.WithLabelValues("membership"))
	beforeRecv := counterValue(t, metrics.PlatformMessagesReceivedTotal.WithLabelValues("membership"))
	beforeFail := counterValue(t, metrics.PlatformMessagesFailedTotal.WithLabelValues("membership"))

	called := false
	h := instrumentConsumer("membership", func(context.Context, events.Envelope[json.RawMessage]) error {
		called = true
		return nil
	})
	err := h(context.Background(), events.Envelope[json.RawMessage]{})

	assert.NoError(t, err)
	assert.True(t, called, "wrapped handler must actually be invoked")
	assert.Equal(t, beforeRecv+1, counterValue(t, metrics.PlatformMessagesReceivedTotal.WithLabelValues("membership")))
	assert.Equal(t, before+1, counterValue(t, metrics.PlatformMessagesProcessedTotal.WithLabelValues("membership")))
	assert.Equal(t, beforeFail, counterValue(t, metrics.PlatformMessagesFailedTotal.WithLabelValues("membership")), "success must not increment failed")
}

func TestInstrumentConsumer_Error_IncrementsReceivedAndFailed_PassesErrorThrough(t *testing.T) {
	ensureMetricsRegistered(t)
	beforeRecv := counterValue(t, metrics.PlatformMessagesReceivedTotal.WithLabelValues("tenant"))
	beforeProc := counterValue(t, metrics.PlatformMessagesProcessedTotal.WithLabelValues("tenant"))
	beforeFail := counterValue(t, metrics.PlatformMessagesFailedTotal.WithLabelValues("tenant"))

	wantErr := errors.New("boom")
	h := instrumentConsumer("tenant", func(context.Context, events.Envelope[json.RawMessage]) error {
		return wantErr
	})
	err := h(context.Background(), events.Envelope[json.RawMessage]{})

	assert.Same(t, wantErr, err, "the handler's own error must pass through unmodified")
	assert.Equal(t, beforeRecv+1, counterValue(t, metrics.PlatformMessagesReceivedTotal.WithLabelValues("tenant")))
	assert.Equal(t, beforeFail+1, counterValue(t, metrics.PlatformMessagesFailedTotal.WithLabelValues("tenant")))
	assert.Equal(t, beforeProc, counterValue(t, metrics.PlatformMessagesProcessedTotal.WithLabelValues("tenant")), "failure must not increment processed")
}

func TestEventschemaByType(t *testing.T) {
	got := eventschemaByType()
	assert.NotEmpty(t, got)
	assert.Equal(t, len(eventschema.ByEventType), len(got))
}

// fakeGlueClient constructs a *glue.Client with a minimal static config —
// no live AWS credentials or network reachability needed for construction
// itself (the SDK client is lazy; a call would fail, but buildGlueCodec's
// empty-schemaNames path below never issues one).
func fakeGlueClient() *glue.Client {
	return glue.New(glue.Options{
		Region:      "ap-south-1",
		Credentials: aws.AnonymousCredentials{},
	})
}

func TestBuildGlueCodec_EmptyRegistryName_ReturnsNoopCodec(t *testing.T) {
	codec, err := buildGlueCodec(context.Background(), fakeGlueClient(), "", nil)
	require.NoError(t, err)
	assert.IsType(t, events.NoopCodec{}, codec)
}

// A non-empty registryName with zero schemaNames never calls Glue (the
// resolve loop has nothing to iterate), so this exercises the full
// success path without any real AWS reachability.
func TestBuildGlueCodec_NonEmptyRegistryName_NoSchemas_Succeeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codec, err := buildGlueCodec(ctx, fakeGlueClient(), "iam-tenant-events", nil)
	require.NoError(t, err)
	assert.NotNil(t, codec)
}

// A non-empty registryName with a non-empty schemaNames list drives
// NewGlueCodec's prefetch loop, which calls out to Glue — pointing the
// client at a closed local port (nothing listens on 127.0.0.1:1) makes that
// call fail fast and deterministically with connection-refused, without any
// live AWS reachability, exercising buildGlueCodec's error-propagation path.
func TestBuildGlueCodec_PrefetchFails_PropagatesError(t *testing.T) {
	unreachable := glue.New(glue.Options{
		Region:           "ap-south-1",
		Credentials:      aws.AnonymousCredentials{},
		BaseEndpoint:     aws.String("http://127.0.0.1:1"),
		RetryMaxAttempts: 1, // fail fast — no retry backoff on a deliberately closed port
	})

	codec, err := buildGlueCodec(context.Background(), unreachable, "iam-tenant-events", []string{"TenantRealmReady"})
	require.Error(t, err)
	assert.Nil(t, codec)
}

func TestBuildSNSPublisher_EmptyTopicARN_ReturnsNoopPublisher(t *testing.T) {
	pub, err := buildSNSPublisher("", events.NoopCodec{}, &fakeLogger{})
	require.NoError(t, err)
	assert.IsType(t, noopPublisher{}, pub)
}

// An invalid ARN is rejected by the SNS publisher's own validation before
// any AWS config/credentials are resolved — testable without live AWS.
func TestBuildSNSPublisher_InvalidTopicARN_ReturnsError(t *testing.T) {
	_, err := buildSNSPublisher("not-a-valid-arn", events.NoopCodec{}, &fakeLogger{})
	require.Error(t, err)
}

func TestNoopPublisher_Publish(t *testing.T) {
	var p noopPublisher
	assert.NoError(t, p.Publish(context.Background(), events.Envelope[json.RawMessage]{}))
}

func TestNoopPublisher_PublishBatch(t *testing.T) {
	var p noopPublisher
	assert.NoError(t, p.PublishBatch(context.Background(), nil))
}

// TestDlqHandler_AlwaysErrors_NeverDeletesTheMessage guards dlqHandler's
// safety-critical contract: platform-events deletes an SQS message only if
// its dead-letter handler returns nil, so a nil return here would silently
// drop a poison message before it ever reaches the queue's -dlq sibling for
// operator inspection. This must never happen, so dlqHandler must always
// return a non-nil error regardless of the envelope it's given.
func TestDlqHandler_AlwaysErrors_NeverDeletesTheMessage(t *testing.T) {
	ensureMetricsRegistered(t)
	before := counterValue(t, metrics.PlatformDLQMessagesTotal.WithLabelValues("billing"))

	err := dlqHandler("billing")(context.Background(), events.Envelope[json.RawMessage]{Type: "AnythingAtAll"})

	require.Error(t, err, "dlqHandler must never return nil — that would delete the message before it reaches -dlq")
	assert.Equal(t, before+1, counterValue(t, metrics.PlatformDLQMessagesTotal.WithLabelValues("billing")))
}
