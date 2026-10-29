// Shared trial realm bootstrap started from main.go (LLD §12.2, RP-INV-2).
// No other code creates the SHARED_TRIAL_REALM realm, yet RP-1 writes every
// trial user into it and AuthZ Enrichment trusts its tenant_id claim (HLD
// §5.4). Each replica converges it once at boot — create if missing, then
// the hardened tender-web client, the tenant_id mapper and the admin-only
// tenant_id user-profile attribute — so a WEB_APP_BASE_URL change or a
// template change reaches it on the next rollout. Concurrent replicas are
// safe: every step is idempotent and a lost create race converges instead.
package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// sharedRealmRetry is the wait between failed attempts.
const sharedRealmRetry = 30 * time.Second

// sharedRealmEnsurer is the one KeycloakAdmin method the loop needs.
type sharedRealmEnsurer interface {
	EnsureSharedRealm(ctx context.Context, realmName string) error
}

// startSharedRealmEnsure runs ensureSharedRealm in the background. It does
// not block startup: a Keycloak outage at boot must not crash-loop the
// server (every route but RP-1 works without this realm), and /readyz
// already reports Keycloak itself.
func startSharedRealmEnsure(ctx context.Context, kc sharedRealmEnsurer, realm string, log port.Logger) {
	go ensureSharedRealm(ctx, kc, realm, sharedRealmRetry, log)
}

// ensureSharedRealm retries EnsureSharedRealm every retry until it succeeds
// or ctx is canceled.
func ensureSharedRealm(ctx context.Context, kc sharedRealmEnsurer, realm string, retry time.Duration, log port.Logger) {
	for {
		err := kc.EnsureSharedRealm(ctx, realm)
		if err == nil {
			log.Info("shared trial realm ensured", map[string]any{"realm": realm})
			return
		}
		log.Error("ensure shared trial realm failed — retrying; trial signups (RP-1) fail until it succeeds",
			map[string]any{"realm": realm, "error": err.Error(), "retry_in": retry.String()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}
