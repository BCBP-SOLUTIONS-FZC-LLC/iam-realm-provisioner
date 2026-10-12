package keycloak

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// tracerName scopes this adapter's spans. RP-INV-1 makes this package the
// only Keycloak caller in the platform, so every keycloak.* span in a trace
// originates here.
const tracerName = "iam-realm-provisioner/keycloak"

// begin opens the observability scope for one logical Admin API operation:
// a `keycloak.<op>` client span plus the op-labeled latency histogram and
// failure counter (§11.2, §11.3). The returned function must be deferred
// with the method's named error so a failure lands on both the span and the
// counter:
//
//	ctx, done := a.begin(ctx, "create_realm")
//	defer func() { done(err) }()
//
// The op values are frozen (§25) — dashboards, the SLO recording rules, and
// the §11.6 alerts all group by them.
func (a *Admin) begin(ctx context.Context, op string) (opCtx context.Context, done func(error)) {
	ctx, span := a.tracer.Start(ctx, "keycloak."+op,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("keycloak.operation", op)),
	)
	start := time.Now()
	return ctx, func(err error) {
		dur := time.Since(start).Seconds()
		outcome := "success" // registry outcome vocabulary: success|error
		if err != nil {
			outcome = "error"
		}
		metrics.KeycloakCallDuration.WithLabelValues(op).Observe(dur)
		metrics.DependencyRequestDuration.WithLabelValues("keycloak", op, outcome).Observe(dur)
		if err != nil {
			metrics.KeycloakCallFailuresTotal.WithLabelValues(op).Inc()
			gincommon.RecordSpanError(ctx, err, err.Error())
		}
		span.End()
	}
}

// unauthorizedTransport watches gocloak's HTTP traffic for the 401 that a
// rotated admin client secret produces and drops the cached token, so the
// next call re-mints it and reports trigger="401" — the signal the
// credential-rotation alert keys on (§11.6). It sits underneath the
// OpenTelemetry round-tripper, which is what injects `traceparent` onto
// these calls.
type unauthorizedTransport struct {
	base   http.RoundTripper
	tokens *tokenCache
}

// RoundTrip implements http.RoundTripper.
func (t unauthorizedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode == http.StatusUnauthorized {
		t.tokens.InvalidateUnauthorized()
	}
	return resp, err
}
