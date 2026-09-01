package port

import "context"

// TxRunner runs fn inside a database transaction, binding the RLS GUC and a
// tx-bound EventPublisher into ctx so services never import the postgres
// adapter directly.
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}
