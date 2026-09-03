package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestNewOTelTracer_EmptyServiceNameDefaults(t *testing.T) {
	tr := NewOTelTracer("")
	require.NotNil(t, tr)
	require.NotNil(t, tr.tracer)
}

func TestOTelTracer_StartSpan_ReturnsEndFn(t *testing.T) {
	tr := NewOTelTracer("iam-realm-provisioner-test")
	ctx, end := tr.StartSpan(context.Background(), "db.query")
	require.NotNil(t, end)
	assert.NotPanics(t, end)
	// Even with the no-op / default provider the returned context carries
	// a span (possibly invalid) — StartSpan must not drop the parent ctx.
	assert.NotNil(t, ctx)
	_ = trace.SpanFromContext(ctx)
}
