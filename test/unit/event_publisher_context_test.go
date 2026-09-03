package unit_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

type stubPublisher struct{}

func (stubPublisher) Enqueue(context.Context, *domain.Event) error { return nil }

func TestEventPublisherFromContext_RoundTrip(t *testing.T) {
	if _, ok := port.EventPublisherFromContext(context.Background()); ok {
		t.Error("expected no publisher in a bare context")
	}

	var p stubPublisher
	ctx := port.WithEventPublisher(context.Background(), p)

	got, ok := port.EventPublisherFromContext(ctx)
	if !ok {
		t.Fatal("expected a publisher to be found")
	}
	if got != port.EventPublisher(p) {
		t.Errorf("got %v, want %v", got, p)
	}
}
