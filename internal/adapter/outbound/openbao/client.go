// Package openbao implements port.OpenBaoClient against OpenBao's KV v2 API
// (Vault-API-compatible) using the deterministic per-tenant IdP secret path
// (RP-D10, §10.2) — no secret reference is ever persisted in Postgres. Auth
// is via OpenBao's Kubernetes auth method (pod ServiceAccount token), a
// genuinely new integration with no existing sibling-service precedent.
package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// Config configures the OpenBao KV v2 client (§12).
type Config struct {
	Addr                string // BAO_ADDR
	AuthRole            string // BAO_AUTH_ROLE
	KVMount             string // BAO_KV_MOUNT ("secret")
	IdpPathPrefix       string // BAO_IDP_PATH_PREFIX ("iam/realm-provisioner/idp")
	KeycloakAdminPath   string // BAO_KEYCLOAK_ADMIN_PATH
	KubernetesTokenPath string // pod ServiceAccount token, defaults to the projected volume path
	HTTPClient          *http.Client
}

// Client implements port.OpenBaoClient.
type Client struct {
	cfg  Config
	http *http.Client
	log  port.Logger

	mu          sync.Mutex
	cachedToken string
	expiresAt   time.Time
}

var _ port.OpenBaoClient = (*Client)(nil)

// New constructs a Client using cfg, defaulting the Kubernetes token path
// and HTTP client when unset.
func New(cfg Config, log port.Logger) *Client {
	if cfg.KubernetesTokenPath == "" {
		cfg.KubernetesTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = httpx.NewClient(8 * time.Second)
	}
	return &Client{cfg: cfg, http: cfg.HTTPClient, log: log}
}

// idpPath is the deterministic KV v2 logical path for (tenantID, idpAlias)
// — computed, never stored (RP-D10).
func (c *Client) idpPath(tenantID uuid.UUID, idpAlias string) string {
	return fmt.Sprintf("%s/%s/%s", c.cfg.IdpPathPrefix, tenantID, idpAlias)
}

// WriteIdpSecret writes secret to the deterministic KV v2 path for
// (tenantID, idpAlias). No reference to this path is ever stored in
// Postgres (RP-D10).
func (c *Client) WriteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string, secret map[string]any) error {
	return c.writeKV(ctx, c.idpPath(tenantID, idpAlias), secret)
}

// ReadIdpSecret reads the secret at (tenantID, idpAlias)'s deterministic
// KV v2 path.
func (c *Client) ReadIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) (map[string]any, error) {
	return c.readKV(ctx, c.idpPath(tenantID, idpAlias))
}

// DeleteIdpSecret permanently deletes the secret at (tenantID, idpAlias)'s
// deterministic KV v2 path (all versions, RP-7).
func (c *Client) DeleteIdpSecret(ctx context.Context, tenantID uuid.UUID, idpAlias string) error {
	return c.deleteKV(ctx, c.idpPath(tenantID, idpAlias))
}

// ── KV v2 primitives ──────────────────────────────────────────────────────

func (c *Client) writeKV(ctx context.Context, logicalPath string, data map[string]any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return fmt.Errorf("openbao: marshal write_secret body: %w", err)
	}
	url := fmt.Sprintf("%s/v1/%s/data/%s", c.cfg.Addr, c.cfg.KVMount, logicalPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapErr("write_secret", err)
	}
	defer closeBody(c.log, "write_secret", resp.Body)
	if resp.StatusCode >= 300 {
		return statusErr("write_secret", resp)
	}
	return nil
}

func (c *Client) readKV(ctx context.Context, logicalPath string) (map[string]any, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/%s/data/%s", c.cfg.Addr, c.cfg.KVMount, logicalPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapErr("read_secret", err)
	}
	defer closeBody(c.log, "read_secret", resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.NewError(domain.ErrIdpConfigNotFound, "no secret at this path")
	}
	if resp.StatusCode >= 300 {
		return nil, statusErr("read_secret", resp)
	}
	var out struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openbao: decode read_secret response: %w", err)
	}
	return out.Data.Data, nil
}

func (c *Client) deleteKV(ctx context.Context, logicalPath string) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	// KV v2 "metadata" delete removes all versions (full delete, not a
	// soft-delete of the latest version only) — RP-7 wants the secret
	// permanently gone, not recoverable via undelete.
	url := fmt.Sprintf("%s/v1/%s/metadata/%s", c.cfg.Addr, c.cfg.KVMount, logicalPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapErr("delete_secret", err)
	}
	defer closeBody(c.log, "delete_secret", resp.Body)
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return statusErr("delete_secret", resp)
	}
	return nil
}

// ── Kubernetes auth ───────────────────────────────────────────────────────

// token returns a cached OpenBao client token, logging in via the
// Kubernetes auth method when absent or expired.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedToken != "" && time.Now().Before(c.expiresAt) {
		return c.cachedToken, nil
	}
	jwt, err := os.ReadFile(c.cfg.KubernetesTokenPath)
	if err != nil {
		return "", fmt.Errorf("openbao: read kubernetes service account token: %w", err)
	}
	body, _ := json.Marshal(map[string]string{ //nolint:errcheck // map[string]string can never fail to marshal
		"role": c.cfg.AuthRole,
		"jwt":  strings.TrimSpace(string(jwt)),
	})
	url := fmt.Sprintf("%s/v1/auth/kubernetes/login", c.cfg.Addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", wrapErr("kubernetes_login", err)
	}
	defer closeBody(c.log, "kubernetes_login", resp.Body)
	if resp.StatusCode >= 300 {
		return "", statusErr("kubernetes_login", resp)
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("openbao: decode kubernetes_login response: %w", err)
	}
	c.cachedToken = out.Auth.ClientToken
	// 30s safety margin, mirroring the Keycloak admin-token cache pattern.
	c.expiresAt = time.Now().Add(time.Duration(out.Auth.LeaseDuration)*time.Second - 30*time.Second)
	return c.cachedToken, nil
}

func wrapErr(op string, err error) error {
	return fmt.Errorf("openbao: %s: %w", op, err)
}

// closeBody closes body, logging (not propagating) a failure — by the time
// this runs the response has already been fully handled, so there is no
// remaining action a Close error could change.
func closeBody(log port.Logger, op string, body io.Closer) {
	if err := body.Close(); err != nil {
		log.Warn("openbao: close response body failed", map[string]any{"op": op, "error": err.Error()})
	}
}

func statusErr(op string, resp *http.Response) error {
	// Best-effort diagnostic read: whatever bytes come back (even zero, on a
	// read error) go straight into the error message, so a read failure
	// here changes nothing about how the caller should react to it.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)) //nolint:errcheck // best-effort diagnostic body read, see comment above
	return fmt.Errorf("openbao: %s: unexpected status %d: %s", op, resp.StatusCode, string(b))
}
