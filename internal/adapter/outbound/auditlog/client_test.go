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
		Metadata:        map[string]any{"validation_outcome": "pass"},
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

func TestWriteEntry_UnencodableMetadataIsRejected(t *testing.T) {
	e := sampleEntry()
	e.Metadata = map[string]any{"bad": make(chan int)}
	err := New("http://example.com", noopLogger{}).WriteEntry(context.Background(), "key-1", e)
	require.ErrorIs(t, err, port.ErrAuditEntryRejected)
}
