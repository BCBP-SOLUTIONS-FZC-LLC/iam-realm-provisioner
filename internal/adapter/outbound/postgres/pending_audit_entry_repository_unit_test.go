//go:build !integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestEnqueueOnce_InvalidKey_ReturnsError covers the audit.RawKey guard
// that fires before any pool access, so a nil pool is safe here.
func TestEnqueueOnce_InvalidKey_ReturnsError(t *testing.T) {
	r := NewPendingAuditEntryRepository(nil, nil)
	err := r.EnqueueOnce(context.Background(), "", domain.AuditEntry{})
	require.ErrorIs(t, err, audit.ErrInvalidKey)
}

// TestEnqueue_NoEnqueuer_ReturnsError: a repository built without the
// platform-audit Enqueuer fails loudly instead of dropping the entry.
func TestEnqueue_NoEnqueuer_ReturnsError(t *testing.T) {
	r := NewPendingAuditEntryRepository(nil, nil)
	err := r.Enqueue(context.Background(), domain.AuditEntry{OccurredAt: time.Now()})
	require.ErrorContains(t, err, "no platform-audit Enqueuer")
}

// failClosed rejects, before touching the pool, an entry for another tenant
// than the one bound on ctx and an entry the library's validation rejects.
func TestEnqueue_FailsClosed(t *testing.T) {
	enq, err := auditoutbox.NewEnqueuer(auditoutbox.EnqueueConfig{SourceService: "iam-realm-provisioner"})
	require.NoError(t, err)
	r := NewPendingAuditEntryRepository(nil, enq).WithMaxMetadataBytes(64)
	tenant := uuid.New()
	valid := domain.AuditEntry{
		TenantID: tenant, EntryType: "config.idp.changed", Action: "update",
		Actor:      domain.AuditActor{Type: "iam_system", ID: "00000000-0000-0000-0000-0000000000a1"},
		Target:     &domain.AuditTarget{Type: "idp", ID: "okta"},
		OccurredAt: time.Now().UTC(), SourceEventType: "TenantIdpConfigChanged",
	}
	g, _ := pgcommon.GUCSetFromContext(context.Background())
	g.TenantID = uuid.NewString()
	other := pgcommon.WithGUCSet(context.Background(), g)
	require.ErrorContains(t, r.Enqueue(other, valid), "does not match the transaction tenant")

	for name, mutate := range map[string]func(*domain.AuditEntry){
		"bad ip":        func(e *domain.AuditEntry) { e.IPAddress = "not-an-ip" },
		"nul":           func(e *domain.AuditEntry) { e.UserAgent = "a\x00b" },
		"metadata cap":  func(e *domain.AuditEntry) { e.Metadata = []byte(`{"k":"` + strings.Repeat("x", 100) + `"}`) },
		"system actor":  func(e *domain.AuditEntry) { e.Actor.ID = uuid.NewString() },
		"zero occurred": func(e *domain.AuditEntry) { e.OccurredAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			e := valid
			mutate(&e)
			require.ErrorContains(t, r.Enqueue(context.Background(), e), "invalid audit entry")
		})
	}
}

// TestToLibraryEntry_MapsEveryField pins the domain → platform-audit entry
// mapping, field for field (the two share the AL-5 wire shape).
func TestToLibraryEntry_MapsEveryField(t *testing.T) {
	tenant := uuid.New()
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	in := domain.AuditEntry{
		TenantID: tenant, EntryType: "config.idp.changed", Action: "update",
		Actor:      domain.AuditActor{Type: "user", ID: uuid.NewString(), Display: "x"},
		Target:     &domain.AuditTarget{Type: "idp", ID: "okta"},
		OccurredAt: at, SourceService: "iam-realm-provisioner", SourceEventType: "TenantIdpConfigChanged",
		IPAddress: "10.0.0.1", UserAgent: "ua", TraceID: "abc", Metadata: []byte(`{"a":1}`),
	}
	got := toLibraryEntry(in)
	assert.Equal(t, audit.Entry{
		TenantID: tenant, EntryType: in.EntryType, Action: in.Action,
		Actor:      audit.Actor{Type: audit.ActorUser, ID: in.Actor.ID, Display: "x"},
		Target:     &audit.Target{Type: "idp", ID: "okta"},
		OccurredAt: at, SourceService: in.SourceService, SourceEventType: in.SourceEventType,
		IPAddress: in.IPAddress, UserAgent: in.UserAgent, TraceID: in.TraceID, Metadata: in.Metadata,
	}, got)

	in.Target = nil
	assert.Nil(t, toLibraryEntry(in).Target)
}

// inbox.NewStore rejects a nil pool before making any network call, so
// NewInboxRepository(nil, nil) causes r.store() to return an error on the
// very first call. This covers the store-error branches in IsProcessed,
// ProcessOnce, and Prune (lines 48, 62, 85 of inbox_repository.go) and
// the NewStore error return in store() itself (line 99).
func TestInboxRepository_NilPool_CausesStoreError(t *testing.T) {
	r := NewInboxRepository(nil, nil) // nil pool → inbox.NewStore returns error
	ctx := context.Background()
	consumer := domain.ConsumerMembership

	t.Run("IsProcessed", func(t *testing.T) {
		_, err := r.IsProcessed(ctx, consumer, uuid.NewString())
		require.Error(t, err, "nil pool must propagate inbox.NewStore's error from store()")
	})

	t.Run("ProcessOnce", func(t *testing.T) {
		_, err := r.ProcessOnce(ctx, consumer, uuid.NewString(), "op", func(context.Context) error { return nil })
		require.Error(t, err)
	})

	t.Run("Prune", func(t *testing.T) {
		_, err := r.Prune(ctx, consumer, 24*time.Hour, 100)
		require.Error(t, err)
	})
}
