package jobs

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// failIdempotency and marshalIdempotencyResult are best-effort helpers
// shared by every job — their own failure path (the ledger write/marshal
// itself misbehaving) is logged, never propagated. Exercised directly here
// since every call site already covers the success path incidentally.

func TestFailIdempotency_PersistFailure_LoggedNotEscalated(t *testing.T) {
	idem := newMockIdempotencyStore()
	idem.FailErr = assert.AnError
	logger := &mockLogger{}
	jctx := &Context{Idempotency: idem, Logger: port.NewSlogStyleLogger(logger)}

	assert.NotPanics(t, func() { failIdempotency(context.Background(), jctx, "some-key") })
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}

func TestMarshalIdempotencyResult_Success(t *testing.T) {
	logger := &mockLogger{}
	jctx := &Context{Logger: port.NewSlogStyleLogger(logger)}
	body := marshalIdempotencyResult(context.Background(), jctx, "test-op", map[string]any{"tenant_id": uuid.New()})
	assert.NotEmpty(t, body)
	assert.Equal(t, 0, logger.warnCalls)
}

func TestMarshalIdempotencyResult_MarshalFailure_LoggedNotEscalated(t *testing.T) {
	logger := &mockLogger{}
	jctx := &Context{Logger: port.NewSlogStyleLogger(logger)}
	// A channel value cannot be marshaled to JSON — exercises the error
	// branch without needing a real job to construct one.
	body := marshalIdempotencyResult(context.Background(), jctx, "test-op", make(chan int))
	assert.Nil(t, body)
	assert.GreaterOrEqual(t, logger.warnCalls, 1)
}
