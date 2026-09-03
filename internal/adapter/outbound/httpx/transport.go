// Package httpx supplies the single HTTP transport every outbound adapter
// in this service builds on, so W3C `traceparent` injection and client-span
// emission cannot be forgotten on a new call site (§11.3). Without it an
// outbound call starts a fresh, parentless trace at the callee and the
// provision → O&M projection chain stops being one trace.
package httpx

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// NewTransport wraps base — nil meaning http.DefaultTransport — with the
// OpenTelemetry round-tripper, which injects the traceparent header carried
// on the request context and records a client span per call. Both
// composition roots call gincommon.InitTracingFromEnv unconditionally, so
// the global propagator and tracer provider are already installed.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base)
}

// NewClient returns an http.Client with the instrumented transport and the
// given per-request timeout.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewTransport(nil)}
}
