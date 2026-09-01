package port

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

type stubContextPublisher struct{}

func (stubContextPublisher) EnqueueCtx(context.Context, *domain.Event) error { return nil }

func TestEventPublisherFromContext_RoundTrip(t *testing.T) {
	if _, ok := EventPublisherFromContext(context.Background()); ok {
		t.Error("expected no publisher in a bare context")
	}

	var p stubContextPublisher
	ctx := WithEventPublisher(context.Background(), p)

	got, ok := EventPublisherFromContext(ctx)
	if !ok {
		t.Fatal("expected a publisher to be found")
	}
	if got != ContextEventPublisher(p) {
		t.Errorf("got %v, want %v", got, p)
	}
}
