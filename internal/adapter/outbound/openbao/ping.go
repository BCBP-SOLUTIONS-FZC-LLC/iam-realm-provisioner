package openbao

import "context"

// Ping checks OpenBao reachability by attempting a token refresh.
// Satisfies the httpadapter.Pinger interface for /readyz — surfaces a
// misconfigured BAO_ADDR or expired ServiceAccount token at pod startup
// rather than at the first RP-6/RP-7 IdP operation (H2).
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.token(ctx)
	return err
}
