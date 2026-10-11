package keycloak

import (
	"context"
	"sync"
	"time"

	"github.com/Nerzal/gocloak/v13"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
)

// adminTokenSafetyMargin is subtracted from the token's expires_in so a
// cached token is proactively refreshed before Keycloak actually rejects
// it (§6.1).
const adminTokenSafetyMargin = 30 * time.Second

// tokenCache caches the Keycloak admin service-account access token with
// single-flight refresh, so a token expiry during a burst (e.g. migrating
// dozens of users at conversion) triggers exactly one refresh (§6.1, RP-D4).
//
// This is the in-process fallback tier: when Valkey is unavailable (or not
// yet wired), correctness is unaffected — only the token-endpoint call
// rate rises, since each of the 2 replicas maintains its own cache.
type tokenCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	inflight  chan struct{} // non-nil while a refresh is in progress
	// unauthorized records that the cached token was dropped because
	// Keycloak rejected it with a 401 rather than because it aged out, so
	// the next mint is attributed to trigger="401" (§11.6).
	unauthorized bool

	client       *gocloak.GoCloak
	adminRealm   string
	clientID     string
	clientSecret string
}

func newTokenCache(client *gocloak.GoCloak, adminRealm, clientID, clientSecret string) *tokenCache {
	return &tokenCache{client: client, adminRealm: adminRealm, clientID: clientID, clientSecret: clientSecret}
}

// Token returns a valid admin access token, refreshing it if absent or
// within adminTokenSafetyMargin of expiry.
func (c *tokenCache) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.expiresAt) {
		tok := c.token
		c.mu.Unlock()
		return tok, nil
	}
	if c.inflight != nil {
		ch := c.inflight
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return c.Token(ctx) // re-check the now-refreshed cache
	}
	ch := make(chan struct{})
	c.inflight = ch
	trigger := "proactive"
	if c.unauthorized {
		trigger = "401"
		c.unauthorized = false
	}
	c.mu.Unlock()

	metrics.AdminTokenRefreshTotal.WithLabelValues(trigger).Inc()
	start := time.Now()
	tok, expiresIn, err := c.refresh(ctx)
	dur := time.Since(start).Seconds()
	outcome := "success" // registry outcome vocabulary: success|error
	if err != nil {
		outcome = "error"
	}
	metrics.KeycloakCallDuration.WithLabelValues("token").Observe(dur)
	metrics.DependencyRequestDuration.WithLabelValues("keycloak", "token", outcome).Observe(dur)
	if err != nil {
		metrics.KeycloakCallFailuresTotal.WithLabelValues("token").Inc()
	}

	c.mu.Lock()
	if err == nil {
		c.token = tok
		c.expiresAt = time.Now().Add(time.Duration(expiresIn)*time.Second - adminTokenSafetyMargin)
	}
	c.inflight = nil
	close(ch)
	c.mu.Unlock()

	return tok, err
}

// Invalidate forces the next Token call to mint a fresh token — used when
// this service knows the token's grants are stale (the post-create realm
// admin grant, §10.2), which is a deliberate re-mint, not a credential
// problem, so it stays on trigger="proactive".
func (c *tokenCache) Invalidate() {
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

// InvalidateUnauthorized drops the cached token after Keycloak rejected it
// with a 401 and attributes the next mint to trigger="401", so a rotated
// admin client secret shows up as a refresh storm rather than as an opaque
// wave of failures (§11.6).
func (c *tokenCache) InvalidateUnauthorized() {
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.unauthorized = true
	c.mu.Unlock()
}

func (c *tokenCache) refresh(ctx context.Context) (accessToken string, expiresIn int, err error) {
	jwt, err := c.client.LoginClient(ctx, c.clientID, c.clientSecret, c.adminRealm)
	if err != nil {
		return "", 0, err
	}
	return jwt.AccessToken, jwt.ExpiresIn, nil
}
