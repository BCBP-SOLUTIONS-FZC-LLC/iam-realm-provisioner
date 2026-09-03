// Package http implements the inbound HTTP surface: Gin handlers and the
// middleware chain that binds gateway-injected identity into the request
// context and the DB pool's RLS GUC. router.go is the single source of
// truth for this service's routes — cmd/server/main.go's job is to
// construct dependencies and call NewRouter, not to encode routing
// decisions itself.
package http

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// Pinger is satisfied by any dependency /readyz must check.
type Pinger interface {
	Health(ctx context.Context) error
}

// DocsConfig controls whether the AsyncAPI catalog surface is exposed.
// Outside production it's always on; in production it's opt-in via Enabled
// and, if AuthToken is set, gated behind a bearer token (§12.1).
type DocsConfig struct {
	Environment string
	Enabled     bool
	AuthToken   string
}

func (d DocsConfig) active() bool {
	return d.Environment != "production" || d.Enabled
}

// RouterConfig bundles every dependency NewRouter needs.
type RouterConfig struct {
	GinConfig gincommon.Config
	Docs      DocsConfig

	Handlers Handlers

	Postgres    Pinger
	SysPostgres Pinger
	Outbox      Pinger
	Keycloak    Pinger
}

// Router owns the Gin engine for this service.
type Router struct {
	engine *gin.Engine
}

// Handler returns the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.engine }

// NewRouter builds and wires every route this service exposes: the
// unauthenticated infra probes, the docs surface, and the mesh-only
// /api/v1/internal/* provisioning API (RP-1..RP-16).
func NewRouter(cfg RouterConfig) *Router {
	errorLogger = cfg.GinConfig.Logger

	r := gin.New()
	r.HandleMethodNotAllowed = true

	// 1 MB body cap.
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		c.Next()
	})
	r.Use(gincommon.TimeoutMiddleware(30 * time.Second))
	r.Use(gincommon.ObservabilityMiddlewares(cfg.GinConfig)...)

	registerInfraRoutes(r, cfg)
	registerDocsRoutes(r, cfg)
	registerInternalRoutes(r, cfg)

	return &Router{engine: r}
}

// ── Infra routes (unauthenticated, registered before auth so LB probes
// with no headers still get 200) ────────────────────────────────────────

type healthHandlers struct {
	postgres    Pinger
	sysPostgres Pinger
	outbox      Pinger
	keycloak    Pinger
}

func registerInfraRoutes(r *gin.Engine, cfg RouterConfig) {
	h := &healthHandlers{postgres: cfg.Postgres, sysPostgres: cfg.SysPostgres, outbox: cfg.Outbox, keycloak: cfg.Keycloak}
	r.GET("/healthz", h.healthz)
	r.GET("/readyz", h.readyz)
	// /metrics is served on its own dedicated port (METRICS_PORT, §3.1) —
	// not on this router — so scraping never shares a listener with the
	// mesh-internal provisioning API.
}

// healthz is a pure liveness check — never inspects dependencies.
func (h *healthHandlers) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz checks Postgres, the outbox runner, and the Keycloak admin-token
// bootstrap (§3.1). SysPostgres is optional (skipped when unset). The checks
// run concurrently — sequentially, a single slow-but-healthy dependency
// could consume the whole readinessProbe timeout budget before a later
// check (e.g. Keycloak) even started, flapping readiness for reasons
// unrelated to that later dependency's own health.
func (h *healthHandlers) readyz(c *gin.Context) {
	ctx := c.Request.Context()

	pingers := []struct {
		name string
		p    Pinger
	}{
		{"database", h.postgres},
		{"sys_database", h.sysPostgres},
		{"outbox", h.outbox},
		{"keycloak", h.keycloak},
	}

	type result struct {
		name string
		ok   bool
	}
	results := make(chan result, len(pingers))
	pending := 0
	for _, pc := range pingers {
		if pc.p == nil {
			continue
		}
		pending++
		go func(name string, p Pinger) {
			results <- result{name: name, ok: p.Health(ctx) == nil}
		}(pc.name, pc.p)
	}

	healthy := true
	checks := gin.H{}
	for i := 0; i < pending; i++ {
		r := <-results
		if r.ok {
			checks[r.name] = "ok"
		} else {
			checks[r.name] = "down"
			healthy = false
		}
	}

	status := http.StatusOK
	overall := "ready"
	if !healthy {
		status = http.StatusServiceUnavailable
		overall = "not ready"
	}
	c.JSON(status, gin.H{"status": overall, "checks": checks})
}

// ── Docs surface ─────────────────────────────────────────────────────────

func registerDocsRoutes(r *gin.Engine, cfg RouterConfig) {
	if !cfg.Docs.active() {
		return
	}

	secHeaders := func(c *gin.Context) {
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	}

	var authMiddleware gin.HandlerFunc = func(c *gin.Context) { c.Next() }
	if cfg.Docs.Environment == "production" && cfg.Docs.AuthToken != "" {
		token := cfg.Docs.AuthToken
		expected := []byte("Bearer " + token)
		authMiddleware = func(c *gin.Context) {
			// Constant-time compare — a plain != leaks how many leading
			// bytes of a guessed token are correct via response latency.
			// ConstantTimeCompare itself returns 0 (no panic) on a length
			// mismatch, so no separate length check is needed first.
			got := []byte(c.GetHeader("Authorization"))
			if subtle.ConstantTimeCompare(got, expected) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		}
	}

	r.GET("/asyncapi", secHeaders, authMiddleware, AsyncAPIHandler)
	r.GET("/asyncapi.yaml", secHeaders, authMiddleware, AsyncAPIYAMLHandler)
}

// ── Provisioning API — /api/v1/internal/* (RP-1..RP-16) ──────────────────

func registerInternalRoutes(r *gin.Engine, cfg RouterConfig) {
	h := cfg.Handlers

	protected := append(
		gincommon.ProtectedMiddlewares(cfg.GinConfig),
		GUCBridgeMiddleware(),
		RequireJSONContentType(),
	)
	// Every route in this service lives under /api/v1/internal — there is
	// almost no public surface (§5.1, §5.4). RequireSystemRole is
	// defense-in-depth; the mesh mTLS + internal-route guard is primary.
	internal := r.Group("/api/v1/internal", append(protected, RequireSystemRole())...)

	idem := RequireIdempotencyKey()

	internal.POST("/tenants/provision-trial", idem, h.ProvisionTrial)         // RP-1
	internal.POST("/tenants/provision-direct", idem, h.ProvisionDirect)       // RP-2
	internal.POST("/tenants/:id/convert", idem, h.Convert)                    // RP-3
	internal.POST("/tenants/:id/revert-conversion", idem, h.RevertConversion) // RP-4
	internal.POST("/tenants/:id/users", idem, h.CreateInvitedUser)            // RP-5
	internal.PUT("/tenants/:id/idp", idem, h.ConfigureIdp)                    // RP-6
	internal.DELETE("/tenants/:id/idp/:alias", h.RemoveIdp)                   // RP-7
	internal.PATCH("/tenants/:id/realm-config", h.ApplyRealmConfig)           // RP-8
	// RP-9's frozen §25 path spells the segment ":uid"; Gin requires one
	// wildcard name per path position across sibling routes sharing this
	// prefix (RP-15/16 use :keycloak_user_id here) — the capture name is
	// an internal routing detail, not part of the wire contract (callers
	// substitute the actual Keycloak user id either way), so this is a
	// non-breaking rename.
	internal.POST("/tenants/:id/users/:keycloak_user_id/mfa-reset", h.ResetMFA)             // RP-9
	internal.POST("/tenants/:id/reactivate", idem, h.Reactivate)                            // RP-10
	internal.GET("/tenants/:id/realm", h.GetRealm)                                          // RP-11
	internal.POST("/tenants/:id/expire-trial", idem, h.ExpireTrial)                         // RP-12
	internal.POST("/tenants/:id/suspend", idem, RequireOperatorRole(), h.Suspend)           // RP-14
	internal.DELETE("/tenants/:id/users/:keycloak_user_id", h.DeleteUser)                   // RP-15
	internal.POST("/tenants/:id/users/:keycloak_user_id/revoke-sessions", h.RevokeSessions) // RP-16

	// Operator-only, whole-estate scan — not tenant-scoped (§5.1).
	internal.POST("/audit/idp-region-portability", RequireOperatorRole(), h.PortabilityAudit) // RP-13
}
