// Package port declares the interfaces the service layer requires from its
// adapters. Everything in this package depends only on core/domain and
// stdlib/third-party value types — never on a concrete adapter, Gin, pgx,
// gocloak, or an AWS SDK package (§3.4, enforced by .go-arch-lint.yml).
package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// EventPublisher enqueues events into the transactional outbox. The insert
// MUST share the same transaction as the business write so state and event
// commit atomically (RP-INV-3).
//
// Callers never pass a pgx.Tx. TxRunner.RunInTx stores the open transaction
// on ctx; the eventbus adapter reads it via postgres.TxFromContext and
// writes outbox_events on that tx — matching iam-org-membership.
type EventPublisher interface {
	Enqueue(ctx context.Context, event *domain.Event) error
}

type contextPublisherKey struct{}

// EventPublisherFromContext retrieves the publisher injected by
// TxRunner.RunInTx.
func EventPublisherFromContext(ctx context.Context) (EventPublisher, bool) {
	p, ok := ctx.Value(contextPublisherKey{}).(EventPublisher)
	return p, ok
}

// WithEventPublisher stores a publisher in ctx. Production TxRunner injects
// the eventbus Publisher after attaching the running tx; tests inject fakes.
func WithEventPublisher(ctx context.Context, p EventPublisher) context.Context {
	return context.WithValue(ctx, contextPublisherKey{}, p)
}
