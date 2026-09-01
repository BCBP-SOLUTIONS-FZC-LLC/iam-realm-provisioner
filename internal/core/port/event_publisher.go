// Package port declares the interfaces the service layer requires from its
// adapters. Everything in this package depends only on core/domain and
// stdlib/third-party value types — never on a concrete adapter, Gin, pgx,
// gocloak, or an AWS SDK package (§3.4, enforced by .go-arch-lint.yml).
package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/jackc/pgx/v5"
)

// EventPublisher enqueues events into the transactional outbox using the
// caller's active pgx.Tx. The insert MUST share the same transaction as the
// business write so state and event commit atomically (RP-INV-3).
type EventPublisher interface {
	Enqueue(ctx context.Context, tx pgx.Tx, event *domain.Event) error
}
