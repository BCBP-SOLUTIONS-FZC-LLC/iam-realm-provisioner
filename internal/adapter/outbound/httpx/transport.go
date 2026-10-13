// Package httpx supplies the single HTTP transport every outbound adapter
// in this service builds on, so W3C `traceparent` injection and client-span
// emission cannot be forgotten on a new call site (§11.3). Without it an
// outbound call starts a fresh, parentless trace at the callee and the
// provision → O&M projection chain stops being one trace.
package httpx

import (
	"io"
	"net/http"
	"strconv"
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

// warner is the minimal logging capability CloseBody needs, defined
// locally rather than as port.Logger — this leaf package may depend on no
// other internal component (§3.4, go-arch-lint's httptransport rule); any
// port.Logger implementation satisfies this structurally.
type warner interface {
	Warn(msg string, fields map[string]any)
}

// ParseRetryAfter parses an HTTP Retry-After header per RFC 9110 §7.1.3,
// accepting either delta-seconds or an HTTP-date. Returns 0 for an empty,
// malformed, or non-positive value; values exceeding ceiling are clamped to
// ceiling. Overflow-safe: a delta-seconds value that would wrap time.Duration
// is capped before the multiplication. now is injected for testability.
func ParseRetryAfter(header string, now time.Time, ceiling time.Duration) time.Duration {
	if header == "" {
		return 0
	}
	maxSecs := int64(ceiling / time.Second)
	if secs, err := strconv.Atoi(header); err == nil {
		switch {
		case secs <= 0:
			return 0
		case int64(secs) > maxSecs:
			return ceiling
		default:
			return time.Duration(secs) * time.Second
		}
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := t.Sub(now); d > 0 {
			if d > ceiling {
				return ceiling
			}
			return d
		}
	}
	return 0
}

// CloseBody closes body, logging (not propagating) a failure — by the time
// a response body is being closed, the caller already has whatever result
// or error it's returning, so a close failure here is not itself actionable.
// The single shared implementation every outbound HTTP adapter defers to.
func CloseBody(log warner, body io.Closer) {
	if err := body.Close(); err != nil {
		log.Warn("httpx: close response body failed", map[string]any{"error": err.Error()})
	}
}
