package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn" // CommandTag — pgcommon.Rows interface method; no pgcommon wrapper
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// fakeTx implements pgcommon.Tx by embedding the nil interface (any method
// other than Exec panics if called) and recording every Exec call — sufficient
// because outbox.Enqueue's InsertRecord calls only tx.Exec (verified by
// reading platform-events' outboxstore.InsertRecord).
type fakeTx struct {
	pgcommon.Tx
	execCalls [][]any
	execErr   error
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls = append(f.execCalls, append([]any{sql}, args...))
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

type failingCodec struct{ err error }

func (f failingCodec) Encode(context.Context, string, []byte) ([]byte, string, error) {
	return nil, "", f.err
}

func TestPublisher_Enqueue_HappyPath_InsertsIntoOutbox(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	tenantID := uuid.New()
	event := &domain.Event{
		Type:     domain.EventTrialTenantProvisioned,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID,
		Data: domain.TrialTenantProvisionedPayload{
			TenantID:       tenantID,
			Realm:          "trial",
			KeycloakUserID: uuid.New(),
		},
	}

	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), event)
	require.NoError(t, err)
	require.Len(t, tx.execCalls, 1, "exactly one INSERT into outbox_events")

	call := tx.execCalls[0]
	sql, ok := call[0].(string)
	require.True(t, ok)
	assert.Contains(t, sql, "INSERT INTO outbox_events")
}

func TestPublisher_Enqueue_RequiresOpenTx(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tenantID := uuid.New()
	err := p.Enqueue(context.Background(), &domain.Event{
		Type:     domain.EventTrialTenantProvisioned,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID,
		Data: domain.TrialTenantProvisionedPayload{
			TenantID:       tenantID,
			Realm:          "trial",
			KeycloakUserID: uuid.New(),
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RunInTx")
}

// TestPublisher_Enqueue_CodecRejectionNeverTouchesOutbox confirms a
// validation failure returns before any Exec call — an invalid payload
// must never reach outbox_events (RP-INV-3 fail-closed enqueue guarantee).
func TestPublisher_Enqueue_CodecRejectionNeverTouchesOutbox(t *testing.T) {
	wantErr := errors.New("schema validation failed")
	p := New(domain.Source, failingCodec{err: wantErr})
	tx := &fakeTx{}

	tenantID := uuid.New()
	event := &domain.Event{
		Type:     domain.EventTrialTenantProvisioned,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID,
		Data:     domain.TrialTenantProvisionedPayload{TenantID: tenantID},
	}

	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), event)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, tx.execCalls, "a codec-rejected payload must never reach the outbox insert")
}

func TestPublisher_Enqueue_ExecFailurePropagates(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	execErr := errors.New("connection reset")
	tx := &fakeTx{execErr: execErr}

	tenantID := uuid.New()
	event := &domain.Event{
		Type:     domain.EventTrialExpired,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID,
		Data:     domain.TrialExpiredPayload{TenantID: tenantID},
	}

	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), event)
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
}

func TestPublisher_WithLogger_ReturnsSamePublisherForChaining(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	got := p.WithLogger(nil)
	assert.Same(t, p, got)
}

// TestPublisher_WithLogger_RoutesExecFailureLogThroughAttachedLogger
// confirms WithLogger isn't merely a no-op setter — the attached logger is
// what Enqueue's ErrorContext call on an outbox Exec failure actually
// writes through, not the zero-value fallback to the top-level slog
// default.
func TestPublisher_WithLogger_RoutesExecFailureLogThroughAttachedLogger(t *testing.T) {
	fl := &fakeWarnLogger{}
	p := New(domain.Source, NoopCodec{}).WithLogger(fl)
	tx := &fakeTx{execErr: errors.New("connection reset")}

	tenantID := uuid.New()
	event := &domain.Event{
		Type:     domain.EventTrialExpired,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID,
		Data:     domain.TrialExpiredPayload{TenantID: tenantID},
	}

	require.Error(t, p.Enqueue(pgadapter.WithTx(context.Background(), tx), event))
	require.Equal(t, 1, fl.errorCount(), "the attached logger, not the slog default, must receive the enqueue failure")
}

// TestPublisher_Enqueue_MarshalErrorPropagates covers Enqueue's very first
// error branch — an event.Data value json.Marshal cannot encode (a channel,
// here) must surface as an error rather than reach the codec or the outbox
// insert.
func TestPublisher_Enqueue_MarshalErrorPropagates(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	event := &domain.Event{
		Type:     domain.EventTrialExpired,
		TenantID: uuid.New(),
		Data:     make(chan int),
	}

	err := p.Enqueue(pgadapter.WithTx(context.Background(), tx), event)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal event payload")
	assert.Empty(t, tx.execCalls)
}

func TestPublisher_Enqueue_IPAddressAndUserAgentPropagateIntoEnvelope(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	tenantID := uuid.New()
	event := &domain.Event{
		Type:      domain.EventTrialExpired,
		TenantID:  tenantID,
		Subject:   tenantID.String(),
		Actor:     domain.SystemActorID,
		IPAddress: "203.0.113.7",
		UserAgent: "test-agent/1.0",
		Data:      domain.TrialExpiredPayload{TenantID: tenantID},
	}

	require.NoError(t, p.Enqueue(pgadapter.WithTx(context.Background(), tx), event))
	require.Len(t, tx.execCalls, 1)

	var envelope struct {
		IPAddress string `json:"ip_address"`
		UserAgent string `json:"user_agent"`
	}
	payloadArg, ok := tx.execCalls[0][3].(json.RawMessage)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, "203.0.113.7", envelope.IPAddress)
	assert.Equal(t, "test-agent/1.0", envelope.UserAgent)
}

// TestPublisher_Enqueue_ValidSpanContextPropagatesTraceAndCorrelationID
// covers the trace-context branch — when the caller's ctx carries a valid
// OTel span, Enqueue must stamp both TraceID and CorrelationID (the span
// ID) onto the envelope so a produced event is traceable back to the
// request that caused it.
func TestPublisher_Enqueue_ValidSpanContextPropagatesTraceAndCorrelationID(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	tenantID := uuid.New()
	event := &domain.Event{
		Type:     domain.EventTrialExpired,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID,
		Data:     domain.TrialExpiredPayload{TenantID: tenantID},
	}

	require.NoError(t, p.Enqueue(pgadapter.WithTx(ctx, tx), event))
	require.Len(t, tx.execCalls, 1)

	var envelope struct {
		TraceID       string `json:"trace_id"`
		CorrelationID string `json:"correlation_id"`
	}
	payloadArg, ok := tx.execCalls[0][3].(json.RawMessage)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, traceID.String(), envelope.TraceID)
	assert.Equal(t, spanID.String(), envelope.CorrelationID)
}

func TestPublisher_Enqueue_MarshalsDataAsPayloadJSON(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	tenantID := uuid.New()
	realmName := "acme-dedicated"
	event := &domain.Event{
		Type:     domain.EventTenantRealmReady,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID,
		Data: domain.TenantRealmReadyPayload{
			TenantID:      tenantID,
			Realm:         realmName,
			KeycloakShard: "shard-1",
			OIDCClients:   []string{"web", "mobile"},
		},
	}
	require.NoError(t, p.Enqueue(pgadapter.WithTx(context.Background(), tx), event))
	require.Len(t, tx.execCalls, 1)

	// The payload argument is the 3rd positional arg after sql ($1=id,
	// $2=event_type, $3=payload, ...) per outboxstore.InsertRecord.
	// domain.OutboxRecord.Payload is declared as json.RawMessage (not
	// []byte) so pgx picks its JSON wire codec by static type — a type
	// assertion to []byte fails even though json.RawMessage's underlying
	// type is []byte, since Go type assertions match the exact dynamic
	// type, not the underlying one.
	call := tx.execCalls[0]
	payloadArg, ok := call[3].(json.RawMessage)
	require.True(t, ok, "expected the marshaled envelope bytes as the payload arg")

	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, domain.EventTenantRealmReady, envelope.Type)
	assert.Contains(t, string(envelope.Data), realmName)
}
