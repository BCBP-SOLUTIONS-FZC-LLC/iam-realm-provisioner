package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

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
	codec, err := buildGlueCodec(context.Background(), fakeGlueClient(), "", nil, &fakeLogger{})
	require.NoError(t, err)
	assert.IsType(t, events.NoopCodec{}, codec)
}

// A non-empty registryName with zero schemaNames never calls Glue (the
// prefetch loop has nothing to iterate), so this exercises the full
// success path — construction, WithLogger, StartRefresher — without any
// real AWS reachability.
func TestBuildGlueCodec_NonEmptyRegistryName_NoSchemas_Succeeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codec, err := buildGlueCodec(ctx, fakeGlueClient(), "iam-tenant-events", nil, &fakeLogger{})
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

	codec, err := buildGlueCodec(context.Background(), unreachable, "iam-tenant-events", []string{"TenantRealmCreated"}, &fakeLogger{})
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
