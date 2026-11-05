// Package s3 implements port.ObjectStore for encrypted realm exports at
// offboarding (§15.2, RP-EVT-4).
package s3

import (
	"bytes"
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// Config configures the S3-backed ObjectStore (§12).
type Config struct {
	Bucket string // REALM_EXPORT_BUCKET
	KMSKey string // REALM_EXPORT_KMS_KEY (alias/iam-realm-export)
}

// putObjectAPI is the narrow seam onto the AWS SDK this adapter actually
// uses — only PutObject, not the full *s3.Client surface — so a fake can
// stand in for tests without a real S3 endpoint. *s3.Client satisfies this
// implicitly.
type putObjectAPI interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// ObjectStore implements port.ObjectStore.
type ObjectStore struct {
	client putObjectAPI
	cfg    Config
}

// New constructs an ObjectStore backed by client, using cfg's bucket and
// KMS key.
func New(client putObjectAPI, cfg Config) *ObjectStore {
	return &ObjectStore{client: client, cfg: cfg}
}

// PutRealmExport writes data to key, KMS-encrypted. Returns only on a
// confirmed durable write (S3 PutObject is synchronously durable once it
// returns success) — realm delete never precedes this call succeeding
// (RP-EVT-4). A 2-minute independent timeout prevents a stall from
// consuming the caller's full reconciler budget (L4).
func (o *ObjectStore) PutRealmExport(ctx context.Context, key string, data []byte) error {
	uploadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ctx = uploadCtx
	input := &s3.PutObjectInput{
		Bucket: aws.String(o.cfg.Bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	}
	if o.cfg.KMSKey != "" {
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(o.cfg.KMSKey)
	}
	if _, err := o.client.PutObject(ctx, input); err != nil {
		return domain.NewError(domain.ErrObjectStoreUnavailable, "realm export upload failed").
			WithDetails(map[string]any{"bucket": o.cfg.Bucket, "key": key, "error": err.Error()})
	}
	return nil
}
