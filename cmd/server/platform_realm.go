// Platform realm bootstrap started from main.go (CD-52 item 2, LLD §12.3).
// A dedicated Keycloak realm for internal platform operators: platform_operator
// role, MFA enforced realm-wide (browser flow OTP subflow set to REQUIRED),
// and short sessions (5-min access token, 30-min idle, 8-h max). Enabled
// only when PLATFORM_REALM_NAME is set; harmless to skip in environments
// where the operator path is not yet wired (pre-CD-52 deps).
//
// Mirrored after shared_realm.go — same retry-until-success pattern, same
// non-blocking startup guarantee, same log contract.
package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// platformRealmRetry is the wait between failed platform realm convergence
// attempts. Owned here independently of sharedRealmRetry so the two loops
// can be tuned separately without touching each other's file.
const platformRealmRetry = 30 * time.Second

// platformRealmEnsurer is the one KeycloakAdmin method the loop needs.
type platformRealmEnsurer interface {
	EnsurePlatformRealm(ctx context.Context, realmName string) error
}

// startPlatformRealmEnsure runs ensurePlatformRealm in the background. It
// does not block startup: a Keycloak outage at boot must not crash-loop the
// server, and routes that do not need the platform realm are unaffected.
func startPlatformRealmEnsure(ctx context.Context, kc platformRealmEnsurer, realm string, log port.Logger) {
	go ensurePlatformRealm(ctx, kc, realm, platformRealmRetry, log)
}

// ensurePlatformRealm retries EnsurePlatformRealm every retry until it
// succeeds or ctx is canceled.
func ensurePlatformRealm(ctx context.Context, kc platformRealmEnsurer, realm string, retry time.Duration, log port.Logger) {
	for {
		err := kc.EnsurePlatformRealm(ctx, realm)
		if err == nil {
			if metrics.PlatformRealmReady != nil {
				metrics.PlatformRealmReady.Set(1)
			}
			log.Info("platform realm ensured", map[string]any{"realm": realm})
			return
		}
		log.Error("ensure platform realm failed — retrying; operator routes fail until it succeeds (CD-52)",
			map[string]any{"realm": realm, "error": err.Error(), "retry_in": retry.String()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}
