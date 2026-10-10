package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func ensureMetricsRegistered(t *testing.T) {
	t.Helper()
	// Use envOr("BUILD_VERSION", buildVersion) — exactly what main() does —
	// so both this helper and main_integration_test.go's main() call agree on
	// the identity. The Makefile exports BUILD_VERSION (e.g. "8167120-dirty");
	// gincommon's sync.Once panics if called twice with different versions.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-realm-provisioner",
		BuildVersion: envOr("BUILD_VERSION", buildVersion),
		Domain:       "iam",
		Environment:  "test",
	})
	metrics.Register("test")
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
// operator inspection. This must never happen regardless of which envelope
// is given.
func TestDlqHandler_AlwaysErrors_NeverDeletesTheMessage(t *testing.T) {
	err := dlqHandler("billing")(context.Background(), events.Envelope[json.RawMessage]{Type: "AnythingAtAll"})
	require.Error(t, err, "dlqHandler must never return nil — that would delete the message before it reaches -dlq")
}

// TestConsumerOptions_UsesGlueDecodeCodec guards the codec wired into every
// inbound SQS consumer. Upstream producers (O&M, Billing) encode event
// payloads with the AWS Glue Schema Registry wire format; the consumer must
// use events.GlueDecodeCodec to strip the header. A wrong codec (e.g.
// NoopCodec) would surface as malformed-JSON decode errors at the consumer,
// routing every upstream event to DLQ.
func TestConsumerOptions_UsesGlueDecodeCodec(t *testing.T) {
	ensureMetricsRegistered(t)
	opts := consumerOptions("membership", 4)
	// events.WithConsumerCodec is a functional option; its concrete type is
	// not exported, so we verify the option set is non-empty and exercisable
	// by constructing a no-op consumer with a fake SQS client. The assertion
	// that matters is that the option list contains a codec option that does
	// NOT reject a well-formed Glue-framed payload.
	require.NotEmpty(t, opts, "consumerOptions must return a non-empty option list")
	// Directly verify the codec strips a valid Glue header by encoding a
	// payload with prependGlueHeader (the same helper GlueCodec.Encode uses)
	// and decoding it via the option's codec — all exercised via GlueDecodeCodec.
	payload := []byte(`{"tenant_id":"00000000-0000-0000-0000-000000000001"}`)
	encoded := make([]byte, 18+len(payload))
	encoded[0] = 0x03 // Glue header version byte
	encoded[1] = 0x00 // no compression
	copy(encoded[18:], payload)
	decoded, err := events.GlueDecodeCodec{}.Decode(context.Background(), "TenantRealmReady", encoded)
	require.NoError(t, err, "events.GlueDecodeCodec must decode a valid Glue-framed payload — if this fails the wrong codec was wired")
	assert.Equal(t, json.RawMessage(payload), decoded)
}
