// Package auditlog implements port.AuditLogClient — this service's
// outbound HTTP dependency on the Audit Log Service's AL-5 direct-write
// ingest (POST /api/v1/internal/audit-entries). It carries the non-bus
// TenantIdpConfigChanged entry (HLD §9.4, §10.6), which is deliberately
// not one of the 9 iam-tenant-events bus events.
package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// maxErrorBody bounds how much of a rejection body is read into the error.
const maxErrorBody = 512

// Client implements port.AuditLogClient.
type Client struct {
	baseURL string
	http    *http.Client
	log     port.Logger
}

var _ port.AuditLogClient = (*Client)(nil)

// New constructs a Client calling Audit Log's internal API at baseURL.
func New(baseURL string, log port.Logger) *Client {
	return &Client{baseURL: baseURL, http: httpx.NewClient(5 * time.Second), log: log}
}

// WriteEntry calls AL-5. 201 (created) and 200 (idempotent replay of
// idempotencyKey) are success; 429 and 5xx are transient; any other
// non-2xx is a permanent rejection wrapping port.ErrAuditEntryRejected.
func (c *Client) WriteEntry(ctx context.Context, idempotencyKey string, entry domain.AuditEntry) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("%w: encode entry: %v", port.ErrAuditEntryRejected, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/internal/audit-entries", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("x-user-id", domain.SystemActorID)
	req.Header.Set("x-tenant-id", entry.TenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")

	resp, err := c.http.Do(req) //nolint:bodyclose // closed via the shared httpx.CloseBody helper below, which the linter's cross-package analysis doesn't trace
	if err != nil {
		return fmt.Errorf("auditlog: AL-5 request: %w", err)
	}
	defer httpx.CloseBody(c.log, resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("auditlog: AL-5 unavailable (status %d)", resp.StatusCode)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody)) //nolint:errcheck // best-effort diagnostic only
		return fmt.Errorf("%w: AL-5 status %d: %s", port.ErrAuditEntryRejected, resp.StatusCode, bytes.TrimSpace(snippet))
	}
}
