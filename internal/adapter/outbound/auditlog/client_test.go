package auditlog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

func sampleEntry() domain.AuditEntry {
	return domain.AuditEntry{
		TenantID: uuid.New(), EntryType: domain.AuditEntryTypeIdpChanged, Action: domain.AuditActionCreate,
		Actor:      domain.AuditActor{Type: domain.AuditActorIAMSystem, ID: domain.SystemActorID},
		Target:     &domain.AuditTarget{Type: "idp", ID: "acme-sso"},
		OccurredAt: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), SourceService: domain.Source,
		SourceEventType: domain.AuditSourceTenantIdpConfigChanged,
		Metadata:        json.RawMessage(`{"validation_outcome":"pass"}`),
	}
}

func TestWriteEntry_CreatedSendsAL5Contract(t *testing.T) {
	entry := sampleEntry()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/internal/audit-entries", r.URL.Path)
		assert.Equal(t, "key-1", r.Header.Get("Idempotency-Key"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, domain.SystemActorID, r.Header.Get("x-user-id"))
		assert.Equal(t, entry.TenantID.String(), r.Header.Get("x-tenant-id"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "config.idp.changed", body["entry_type"])
		assert.Equal(t, "TenantIdpConfigChanged", body["source_event_type"])
		assert.Equal(t, "iam-realm-provisioner", body["source_service"])
		assert.Equal(t, entry.TenantID.String(), body["tenant_id"])
		assert.Equal(t, "2026-09-24T10:00:00Z", body["occurred_at"])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	require.NoError(t, New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", entry))
}

func TestWriteEntry_ReplayIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	require.NoError(t, New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry()))
}

func TestWriteEntry_TransientStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		err := New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
		srv.Close()
		require.Error(t, err, "status %d", status)
		assert.False(t, errors.Is(err, port.ErrAuditEntryRejected), "status %d must be transient", status)
	}
}

// AL-5 429 with a delta-seconds Retry-After hint must surface as a
// *port.RetryAfterError so the drainer honors AL's backoff instead of the
// row's default exponential ClaimDue backoff.
func TestWriteEntry_429WithRetryAfterSecondsSurfacesTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.Error(t, err)
	var rae *port.RetryAfterError
	require.ErrorAs(t, err, &rae)
	assert.Equal(t, 42*time.Second, rae.RetryAfter)
	assert.False(t, errors.Is(err, port.ErrAuditEntryRejected))
}

// A 429 without a Retry-After header falls through to the plain transient
// error; the row keeps its default exponential backoff.
func TestWriteEntry_429WithoutRetryAfterIsPlainTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.Error(t, err)
	var rae *port.RetryAfterError
	assert.False(t, errors.As(err, &rae), "no Retry-After header means no typed error")
}

// A hostile Retry-After hint beyond the 5-minute cap must be clamped so a
// misconfigured upstream can never stall delivery arbitrarily.
func TestWriteEntry_429RetryAfterClampedToMaxBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "36000") // 10 hours
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.Error(t, err)
	var rae *port.RetryAfterError
	require.ErrorAs(t, err, &rae)
	assert.Equal(t, maxRetryAfter, rae.RetryAfter)
}

// Malformed and non-positive Retry-After values are ignored (no typed
// error), so ordinary backoff applies.
func TestParseRetryAfter_Malformed(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	assert.Zero(t, parseRetryAfter("", now))
	assert.Zero(t, parseRetryAfter("nonsense", now))
	assert.Zero(t, parseRetryAfter("0", now))
	assert.Zero(t, parseRetryAfter("-5", now))
	assert.Zero(t, parseRetryAfter(now.Add(-1*time.Hour).UTC().Format(http.TimeFormat), now),
		"an HTTP-date in the past yields zero delay, not a negative one")
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	got := parseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now)
	assert.InDelta(t, 90*time.Second, got, float64(time.Second),
		"HTTP-date Retry-After decodes to duration relative to now")
}

func TestWriteEntry_PermanentRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"unknown_entry_type"}`))
	}))
	defer srv.Close()
	err := New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.ErrorIs(t, err, port.ErrAuditEntryRejected)
	assert.Contains(t, err.Error(), "unknown_entry_type")
}

func TestWriteEntry_ConnectionErrorIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	err := New(url, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrAuditEntryRejected))
}

func TestWriteEntry_InvalidBaseURL(t *testing.T) {
	err := New("http://bad host", noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.Error(t, err)
}

func TestWriteEntry_NilTenantIDIsRejected(t *testing.T) {
	e := sampleEntry()
	e.TenantID = uuid.UUID{}
	err := New("http://example.com", noopLogger{}).WriteEntry(context.Background(), "key-1", e)
	require.ErrorIs(t, err, port.ErrAuditEntryRejected)
}

func TestWriteEntry_NilMetadataIsOmitted(t *testing.T) {
	e := sampleEntry()
	e.Metadata = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, hasMetadata := body["metadata"]
		assert.False(t, hasMetadata, "nil Metadata must be omitted from the request body")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	require.NoError(t, New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", e))
}

// AL-5's RFC 9457 422 UNKNOWN_ENTRY_TYPE is deploy skew (Audit Log does not
// know the type yet), surfaced as ErrAuditEntryTypeUnknown — NOT a
// permanent rejection — so the drainer keeps and retries the row.
func TestWriteEntry_UnknownEntryTypeIsNotAPermanentRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"type":"https://errors.company.com/iam-audit-log/unknown-entry-type","status":422,"errorCode":"UNKNOWN_ENTRY_TYPE"}`))
	}))
	defer srv.Close()
	err := New(srv.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.ErrorIs(t, err, port.ErrAuditEntryTypeUnknown)
	assert.False(t, errors.Is(err, port.ErrAuditEntryRejected))

	// Any other 422 (e.g. METADATA_TOO_LARGE) stays permanent.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":422,"errorCode":"METADATA_TOO_LARGE"}`))
	}))
	defer srv2.Close()
	err = New(srv2.URL, noopLogger{}).WriteEntry(context.Background(), "key-1", sampleEntry())
	require.ErrorIs(t, err, port.ErrAuditEntryRejected)
}

// The drainer's AL-5 body for RP-1/RP-2's TermsAccepted entry, exactly as
// Audit Log rev 0.31 expects it, under the deterministic key.
func TestWriteEntry_TermsAcceptedSendsAL5Contract(t *testing.T) {
	tenantID, owner := uuid.New(), uuid.New()
	acceptedAt := time.Date(2026, 10, 22, 9, 12, 4, 0, time.UTC)
	entry := domain.TermsAcceptedAuditEntry(tenantID, owner, domain.TermsAcceptance{
		Version: "2026-09", AcceptedAt: acceptedAt, IPAddress: "2001:db8::7", UserAgent: "Mozilla/5.0 (Macintosh)",
	}, domain.SignupTypeDirectPaid, "enterprise", "0af7651916cd43dd8448eb211c80319c")
	key := domain.TermsAcceptedIdempotencyKey(tenantID, owner, "2026-09")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "TermsAccepted:"+tenantID.String()+":"+owner.String()+":2026-09", r.Header.Get("Idempotency-Key"))
		assert.Equal(t, tenantID.String(), r.Header.Get("x-tenant-id"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, map[string]any{
			"tenant_id": tenantID.String(), "entry_type": "legal.terms.accepted", "action": "accept",
			"actor":       map[string]any{"type": "user", "id": owner.String()},
			"target":      map[string]any{"type": "tenant", "id": tenantID.String()},
			"occurred_at": "2026-10-22T09:12:04Z", "source_service": "iam-realm-provisioner",
			"source_event_type": "TermsAccepted", "ip_address": "2001:db8::7", "user_agent": "Mozilla/5.0 (Macintosh)",
			"trace_id": "0af7651916cd43dd8448eb211c80319c",
			"metadata": map[string]any{"terms_version": "2026-09", "accepted_at": "2026-10-22T09:12:04Z",
				"signup_type": "direct_paid", "plan": "enterprise"},
		}, body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	require.NoError(t, New(srv.URL, noopLogger{}).WriteEntry(context.Background(), key, entry))
}
