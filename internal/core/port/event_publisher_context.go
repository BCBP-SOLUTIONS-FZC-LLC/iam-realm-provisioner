package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

type contextPublisherKey struct{}

// ContextEventPublisher is the tx-bound view of EventPublisher that services
// see inside a RunInTx block. TxRunner injects the concrete implementation
// into the context so services never need to touch pgx.Tx directly — keeps
// the service layer free of infrastructure imports.
type ContextEventPublisher interface {
	EnqueueCtx(ctx context.Context, event *domain.Event) error
}

// EventPublisherFromContext retrieves the tx-bound publisher injected by
// TxRunner.RunInTx.
func EventPublisherFromContext(ctx context.Context) (ContextEventPublisher, bool) {
	p, ok := ctx.Value(contextPublisherKey{}).(ContextEventPublisher)
	return p, ok
}

// WithEventPublisher stores a tx-bound publisher in ctx.
func WithEventPublisher(ctx context.Context, p ContextEventPublisher) context.Context {
	return context.WithValue(ctx, contextPublisherKey{}, p)
}
