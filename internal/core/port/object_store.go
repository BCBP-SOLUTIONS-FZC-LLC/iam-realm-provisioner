package port

import "context"

// ObjectStore is the outbound seam onto S3 for encrypted realm exports at
// offboarding (§15.2, RP-EVT-4). Realm delete never precedes a confirmed
// durable write here.
type ObjectStore interface {
	// PutRealmExport writes data (a realm export) to key, KMS-encrypted.
	// Returns only on a confirmed durable write.
	PutRealmExport(ctx context.Context, key string, data []byte) error
}
