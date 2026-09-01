// Package eventbus implements the outbound port.EventPublisher (writes to
// the transactional outbox, RP-INV-3) and the wire-format codecs used at
// SNS-publish time (Glue Schema Registry) and at SQS-consume time (decoding
// a Glue-encoded payload from another producer).
package eventbus

import "context"

// Codec is the enqueue-time schema-validation hook (payload shape only —
// no wire encoding happens here; that is deferred to the outbox runner's
// SNS publisher via events.WithCodec, so the outbox always stores
// human-readable plain JSON).
type Codec interface {
	Encode(ctx context.Context, schemaName string, payload []byte) (encoded []byte, schemaVersionID string, err error)
}

// NoopCodec passes payload through unchanged — the base every enqueue-time
// codec wraps.
type NoopCodec struct{}

// Encode returns payload unchanged, performing no schema validation or wire
// encoding.
func (NoopCodec) Encode(_ context.Context, _ string, payload []byte) (encoded []byte, schemaVersionID string, err error) {
	return payload, "", nil
}
