package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

const (
	validTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	validSpanID  = "00f067aa0ba902b7"
)

func envelopeWith(traceID, correlationID string) events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{Type: "TenantCreated", TraceID: traceID, CorrelationID: correlationID}
}

func TestOriginLink_ValidIDs_LinksToProducerSpan(t *testing.T) {
	link, ok := originLink(envelopeWith(validTraceID, validSpanID))
	require.True(t, ok)
	assert.Equal(t, validTraceID, link.SpanContext.TraceID().String())
	assert.Equal(t, validSpanID, link.SpanContext.SpanID().String())
	assert.True(t, link.SpanContext.IsRemote())
}

func TestOriginLink_MissingOrMalformedIDs_NoLink(t *testing.T) {
	cases := map[string]events.Envelope[json.RawMessage]{
		"no ids":               envelopeWith("", ""),
		"malformed trace id":   envelopeWith("not-hex", validSpanID),
		"correlation not span": envelopeWith(validTraceID, "8c1f0a6e-5d1c-4f8e-9f0a-2b3c4d5e6f70"),
		"zero trace id":        envelopeWith("00000000000000000000000000000000", validSpanID),
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok := originLink(env)
			assert.False(t, ok)
		})
	}
}

// The span wrapper is transparent: the handler runs with the envelope and
// its error comes back unchanged, with or without a producer link.
func TestInstrumentedHandler_PassesThroughResult(t *testing.T) {
	boom := errors.New("boom")
	for _, env := range []events.Envelope[json.RawMessage]{envelopeWith(validTraceID, validSpanID), envelopeWith("", "")} {
		called := 0
		h := instrumentedHandler("tenant-realm-q", func(ctx context.Context, got events.Envelope[json.RawMessage]) error {
			called++
			assert.Equal(t, env.Type, got.Type)
			return boom
		})
		assert.ErrorIs(t, h(context.Background(), env), boom)
		assert.Equal(t, 1, called)
	}
}
