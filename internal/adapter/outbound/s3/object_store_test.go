package s3

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// fakePutObjectAPI is a minimal stand-in for the AWS SDK's *s3.Client,
// satisfying putObjectAPI so PutRealmExport can be tested without a real
// S3 endpoint.
type fakePutObjectAPI struct {
	input *s3.PutObjectInput
	err   error
}

func (f *fakePutObjectAPI) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.input = params
	if f.err != nil {
		return nil, f.err
	}
	return &s3.PutObjectOutput{}, nil
}

func TestPutRealmExport_Success_PassesThroughBucketKeyKMSAndBody(t *testing.T) {
	fake := &fakePutObjectAPI{}
	store := New(fake, Config{Bucket: "iam-realm-exports", KMSKey: "alias/iam-realm-export"})

	err := store.PutRealmExport(context.Background(), "tenant-42/realm-export.json", []byte(`{"realm":"tenant-42"}`))
	require.NoError(t, err)

	require.NotNil(t, fake.input)
	assert.Equal(t, "iam-realm-exports", aws.ToString(fake.input.Bucket))
	assert.Equal(t, "tenant-42/realm-export.json", aws.ToString(fake.input.Key))
	assert.Equal(t, types.ServerSideEncryptionAwsKms, fake.input.ServerSideEncryption)
	assert.Equal(t, "alias/iam-realm-export", aws.ToString(fake.input.SSEKMSKeyId))

	body, err := io.ReadAll(fake.input.Body)
	require.NoError(t, err)
	assert.Equal(t, `{"realm":"tenant-42"}`, string(body))
}

func TestPutRealmExport_NoKMSKeyConfigured_OmitsEncryptionParams(t *testing.T) {
	fake := &fakePutObjectAPI{}
	store := New(fake, Config{Bucket: "iam-realm-exports"})

	require.NoError(t, store.PutRealmExport(context.Background(), "k", []byte("x")))

	require.NotNil(t, fake.input)
	assert.Empty(t, fake.input.ServerSideEncryption)
	assert.Nil(t, fake.input.SSEKMSKeyId)
}

func TestPutRealmExport_S3Error_SurfacesAsObjectStoreUnavailableDomainError(t *testing.T) {
	fake := &fakePutObjectAPI{err: errors.New("simulated S3 outage")}
	store := New(fake, Config{Bucket: "iam-realm-exports", KMSKey: "alias/iam-realm-export"})

	err := store.PutRealmExport(context.Background(), "tenant-42/realm-export.json", []byte("payload"))
	require.Error(t, err)

	var domErr *domain.Error
	require.ErrorAs(t, err, &domErr)
	assert.Equal(t, domain.ErrObjectStoreUnavailable, domErr.Code)
	assert.Equal(t, 503, domErr.Status())
	assert.Equal(t, "iam-realm-exports", domErr.Details["bucket"])
	assert.Equal(t, "tenant-42/realm-export.json", domErr.Details["key"])
	assert.Contains(t, domErr.Details["error"], "simulated S3 outage")
}
