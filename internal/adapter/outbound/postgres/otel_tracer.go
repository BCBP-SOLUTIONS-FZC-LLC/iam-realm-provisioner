package postgres

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// OTelTracer adapts the process-global TracerProvider that
// gincommon.InitTracingFromEnv / ObservabilityMiddlewares install onto
// pgcommon.Config.Tracer (StartSpan). Every db.query span therefore
// exports through the same OTLP pipeline as HTTP spans — matching the
// sibling iam-org-membership wiring.
type OTelTracer struct {
	tracer trace.Tracer
}

// NewOTelTracer returns a pgcommon.Config.Tracer backed by gincommon's
// global TracerProvider. serviceName becomes the OTel instrumentation
// scope (typically gincommon.Config.ServiceName).
func NewOTelTracer(serviceName string) *OTelTracer {
	if serviceName == "" {
		serviceName = "iam-realm-provisioner"
	}
	return &OTelTracer{tracer: otel.Tracer(serviceName)}
}

// StartSpan starts a span named name and returns the child context together
// with the span's end function.
func (o *OTelTracer) StartSpan(ctx context.Context, name string) (spanCtx context.Context, end func()) {
	ctx, span := o.tracer.Start(ctx, name)
	return ctx, func() { span.End() }
}
