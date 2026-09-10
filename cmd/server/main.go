// Package main is the composition root for the Realm Provisioner service's
// server process: the HTTP API (RP-1..RP-16), the outbox.Runner, and the
// three inbound SQS consumers (§3.1).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	_ "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/docs/swagger"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/consumer"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/http"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	keycloakadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/openbao"
	orgmembershipclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/orgmembership"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	s3adapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/s3"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgmetrics"
)

// buildVersion is injected by -ldflags at build time (see Dockerfile / Makefile).
var buildVersion = "dev"

func main() {
	appEnv := envOr("APP_ENV", "dev")
	if appEnv != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}

	validateRequiredEnv(appEnv)

	// ── 1. Logger — Zap via platform-gincommon ────────────────────────────
	log, err := logger.NewLogger(appEnv)
	if err != nil {
		panic("init logger: " + err.Error())
	}

	// ── 2. Tracing — always install gincommon's TracerProvider so in-process
	// spans get valid trace IDs even when OTEL_EXPORTER_OTLP_ENDPOINT is unset
	// (dev). ObservabilityMiddlewares' EnsureInitTelemetry is then a no-op.
	shutdownTracing := gincommon.InitTracingFromEnv()

	cfg := gincommon.Config{
		Logger:       log,
		ServiceName:  envOr("APP_NAME", "iam-realm-provisioner"),
		BuildVersion: envOr("BUILD_VERSION", buildVersion),
	}
	// ObservabilityMiddlewares is gincommon's public metrics-init API. Call
	// it here (before any collector registration or exporter goroutine) so
	// business / events / pgcommon metrics land on gincommon.MetricsRegisterer
	// with matching {service, version} const labels. NewRouter applies the
	// same middleware slice to the Gin engine; gincommon's metrics.Init is sync.Once.
	_ = gincommon.ObservabilityMiddlewares(cfg)

	metrics.Register(appEnv)

	// platform-events outbox/publish/consume metrics and platform-pgcommon
	// query/pool metrics share gincommon's registerer so a single /metrics
	// scrape (promhttp on METRICS_PORT) serves HTTP + business + outbox +
	// pg collectors together.
	events.InitWithRegisterer(cfg.ServiceName, cfg.BuildVersion, gincommon.MetricsRegisterer())
	pgmetrics.InitWithRegisterer(cfg.ServiceName, cfg.BuildVersion, gincommon.MetricsRegisterer())

	// ── 3. Database — pgcommon.ConfigFromEnv reads DATABASE_URL/PG_* directly
	// so pool sizing, PgBouncer mode, and DSN assembly have exactly one
	// implementation instead of a second one hand-rolled here. ────────────
	queryTracer := pgadapter.NewOTelTracer(cfg.ServiceName)

	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	// DSNFromEnv (not a bare pgCfg.DSN) so the DATABASE_URL bypass applies
	// here too: PG_STATEMENT_TIMEOUT must be ignored when DATABASE_URL is
	// set verbatim, per DSNFromEnv/ApplyStatementTimeout's own contract.
	dsn := pgadapter.DSNFromEnv()
	// Migrations must bypass PgBouncer because the migration runner acquires
	// a pg_advisory_lock, which is session-scoped and breaks under
	// transaction pooling. MIGRATION_DATABASE_URL points directly at
	// Postgres; falls back to dsn when unset.
	migrationDSN := pgadapter.MigrationDSNFromEnv()

	pgCfg.DSN = dsn
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(log)
	pgCfg.Tracer = queryTracer
	pool, err := pgcommon.NewPool(context.Background(), pgCfg)
	if err != nil {
		panic(fmt.Sprintf("connect to postgres: %v", err))
	}
	defer pool.Close()

	// sysPool: BYPASSRLS pool used by the reconciler jobs and cross-tenant
	// sweeps. In production SYSTEM_DATABASE_URL must point to
	// realm_provisioner_migrator (§10.1). Falls back to DSNFromEnv() for
	// dev — cross-tenant queries then RLS-filter to zero rows (safe no-op).
	sysDSN := pgadapter.SystemDSNFromEnv()
	// SystemPoolConfig reads ConfigFromEnv internally, forces PGBouncerMode,
	// and wires the gincommon logger — matching iam-org-membership. Tracer
	// is set here so db.query spans export through gincommon's provider.
	sysCfg := pgadapter.SystemPoolConfig(sysDSN, log)
	sysCfg.Tracer = queryTracer
	sysPool, err := pgcommon.NewPool(context.Background(), sysCfg)
	if err != nil {
		panic(fmt.Sprintf("connect sysPool: %v", err))
	}
	defer sysPool.Close()
	if sysDSN == dsn {
		log.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant queries will be RLS-filtered", nil)
	}

	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	// Migration order is critical (§19): outbox.ApplySchema creates
	// outbox_events (JSONB payload) BEFORE the domain migration, which
	// ALTERs that column to text.
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: migrationDSN}); err != nil {
		panic(fmt.Sprintf("outbox schema: %v", err))
	}
	if err := pgadapter.RunMigrations(ctx, migrationDSN, log); err != nil {
		panic(fmt.Sprintf("domain migrations: %v", err))
	}

	// ── 4. AWS clients ────────────────────────────────────────────────────
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(envOr("AWS_REGION", "ap-south-1")),
	)
	if err != nil {
		panic(fmt.Sprintf("load aws config: %v", err))
	}
	var sqsOpts []func(*sqs.Options)
	var glueOpts []func(*glue.Options)
	var s3Opts []func(*s3.Options)
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		sqsOpts = append(sqsOpts, func(o *sqs.Options) { o.BaseEndpoint = &ep })
		glueOpts = append(glueOpts, func(o *glue.Options) { o.BaseEndpoint = &ep })
		s3Opts = append(s3Opts, func(o *s3.Options) { o.UsePathStyle = true; o.BaseEndpoint = &ep })
	}
	sqsClient := sqs.NewFromConfig(awsCfg, sqsOpts...)
	glueClient := glue.NewFromConfig(awsCfg, glueOpts...)
	s3Client := s3.NewFromConfig(awsCfg, s3Opts...)

	// ── 5. Event codec + outbox publisher (single topic, RP publishes only
	// to iam-tenant-events — no RoutingPublisher needed, §7.3.1) ─────────
	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		panic(fmt.Sprintf("init validating codec: %v", err))
	}
	outboxPublisher := eventbusadapter.New(cfg.ServiceName, enqueueCodec).WithLogger(log)
	txRunner := pgadapter.NewTxRunner(pool, outboxPublisher)

	schemaNames := make([]string, 0, 9)
	for name := range eventschemaByType() {
		schemaNames = append(schemaNames, name)
	}
	snsCodec, err := buildGlueCodec(ctx, glueClient, os.Getenv("GLUE_REGISTRY_TENANT_NAME"), schemaNames, log)
	if err != nil {
		panic(fmt.Sprintf("init glue codec: %v", err))
	}
	snsPublisher, err := buildSNSPublisher(os.Getenv("SNS_TOPIC_TENANT_ARN"), snsCodec, log)
	if err != nil {
		panic(fmt.Sprintf("build sns publisher: %v", err))
	}

	// ── 6. Outbox runner ─────────────────────────────────────────────────
	outboxRunner, err := outbox.NewRunner(outbox.Config{
		Pool:               pool,
		Publisher:          snsPublisher,
		Logger:             log,
		PollInterval:       envDuration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
		BatchSize:          envInt("OUTBOX_BATCH_SIZE", 50),
		MaxAttempts:        envInt("OUTBOX_MAX_ATTEMPTS", 5),
		DrainTimeout:       envDuration("OUTBOX_DRAIN_TIMEOUT", 30*time.Second),
		PublishConcurrency: envInt("OUTBOX_PUBLISH_CONCURRENCY", 4),
		PublishTimeout:     envDuration("OUTBOX_PUBLISH_TIMEOUT", 10*time.Second),
		StartupJitter:      envDuration("OUTBOX_STARTUP_JITTER", 2*time.Second),
		ClaimLeaseDuration: envDuration("OUTBOX_CLAIM_LEASE_DURATION", 10*time.Minute),
	})
	if err != nil {
		panic(fmt.Sprintf("create outbox runner: %v", err))
	}
	go func() {
		if err := outboxRunner.Start(ctx); err != nil {
			log.Error("outbox runner stopped", map[string]interface{}{"error": err.Error()})
		}
	}()

	// ── 7. Outbound adapters (Keycloak — RP-INV-1 sole seam — OpenBao,
	// Org & Membership, S3) ──────────────────────────────────────────────
	keycloakAdmin := keycloakadapter.New(keycloakadapter.Config{
		BaseURL:      mustEnv("KEYCLOAK_BASE_URL", appEnv),
		AdminRealm:   envOr("KEYCLOAK_ADMIN_REALM", "master"),
		ClientID:     envOr("KEYCLOAK_ADMIN_CLIENT_ID", "iam-realm-provisioner"),
		ClientSecret: mustEnv("KEYCLOAK_ADMIN_CLIENT_SECRET", appEnv),
		RateLimitRPS: envInt("KEYCLOAK_RATE_LIMIT_RPS", 20),
		Timeout:      envDuration("KEYCLOAK_TIMEOUT_MS", 8*time.Second),
	}, log)

	openbaoClient := openbao.New(openbao.Config{
		Addr:              envOr("BAO_ADDR", "https://openbao.iam.svc.cluster.local:8200"),
		AuthRole:          envOr("BAO_AUTH_ROLE", "iam-realm-provisioner"),
		KVMount:           envOr("BAO_KV_MOUNT", "secret"),
		IdpPathPrefix:     envOr("BAO_IDP_PATH_PREFIX", "iam/realm-provisioner/idp"),
		KeycloakAdminPath: envOr("BAO_KEYCLOAK_ADMIN_PATH", "iam/realm-provisioner/keycloak-admin"),
	}, log)

	orgMembershipClient := orgmembershipclient.New(mustEnv("ORG_MEMBERSHIP_BASE_URL", appEnv), log)

	objectStore := s3adapter.New(s3Client, s3adapter.Config{
		Bucket: envOr("REALM_EXPORT_BUCKET", "iam-realm-exports"),
		KMSKey: envOr("REALM_EXPORT_KMS_KEY", "alias/iam-realm-export"),
	})
	_ = objectStore // wired into the RP-C3 lapse-sweep reconciler (Phase 6)

	// ── 8. Repositories (Phase 1) ─────────────────────────────────────────
	tenantRealmRepo := pgadapter.NewTenantRealmRepository(pool)
	idpConfigRepo := pgadapter.NewIdpConfigRepository(pool)
	idpValidationRepo := pgadapter.NewIdpValidationRepository(pool)
	reconRepo := pgadapter.NewReconciliationRepository(pool)
	idempotencyRepo := pgadapter.NewIdempotencyRepository(pool)
	processedEventsRepo := pgadapter.NewProcessedEventsRepository(pool)

	// ── 8b. Core services ─────────────────────────────────────────────────
	sharedRealm := envOr("SHARED_TRIAL_REALM", "trial")
	provisioningSvc := service.NewProvisioningService(
		tenantRealmRepo, idempotencyRepo, keycloakAdmin, txRunner, sharedRealm, nil,
	).WithLogger(log)
	lifecycleSvc := service.NewLifecycleService(
		tenantRealmRepo, idempotencyRepo, keycloakAdmin, txRunner, sharedRealm,
	).WithLogger(log)
	mfaSvc := service.NewMfaService(tenantRealmRepo, keycloakAdmin, orgMembershipClient).WithLogger(log)
	idpSvc := service.NewIdpService(tenantRealmRepo, idpConfigRepo, idpValidationRepo, idempotencyRepo, openbaoClient, keycloakAdmin).WithLogger(log)
	realmConfigSvc := service.NewRealmConfigService(tenantRealmRepo, reconRepo, keycloakAdmin).WithLogger(log)

	internalH := httpadapter.NewInternalHandler(provisioningSvc, lifecycleSvc, mfaSvc, idpSvc, realmConfigSvc)

	// ── 9. Inbound SQS consumers (§7.1) — wired with a durable no-op
	// handler (§7.1). ─────────────────────────────────────────────────────
	// txRunner so every consumer's processed_events mark joins withPool
	// (IDEMP-2), matching iam-org-membership. Membership has no local
	// write; Tenant/Billing commit the projection write in the same tx.
	membershipConsumer := consumeradapter.NewMembershipConsumer(mfaSvc, processedEventsRepo, txRunner, log)
	tenantConsumer := consumeradapter.NewTenantConsumer(tenantRealmRepo, processedEventsRepo, txRunner, log)
	billingConsumer := consumeradapter.NewBillingConsumer(tenantRealmRepo, keycloakAdmin, processedEventsRepo, txRunner, log)

	var sqsConsumers []events.Consumer
	consumerCfgs := []struct {
		envVar      string
		concurrency string
		defaultConc int
		label       string
		handler     events.Handler
	}{
		{"SQS_MEMBERSHIP_REALM_QUEUE_URL", "SQS_MEMBERSHIP_REALM_CONCURRENCY", 4, "membership-realm-q", membershipConsumer.Handle},
		{"SQS_TENANT_REALM_QUEUE_URL", "SQS_TENANT_REALM_CONCURRENCY", 4, "tenant-realm-q", tenantConsumer.Handle},
		{"SQS_BILLING_REALM_QUEUE_URL", "SQS_BILLING_REALM_CONCURRENCY", 2, "billing-realm-q", billingConsumer.Handle},
	}
	for _, cc := range consumerCfgs {
		url := os.Getenv(cc.envVar)
		if url == "" {
			log.Warn(cc.label+" consumer disabled — queue URL unset", nil)
			continue
		}
		cons, err := events.NewSQSConsumerWithClient(
			events.SQSConfig{QueueURL: url, Region: envOr("AWS_REGION", "ap-south-1"), Logger: log},
			sqsClient,
			cc.handler,
			events.WithConsumerCodec(eventbusadapter.GlueDecodeCodec{}),
			events.WithConcurrency(envInt(cc.concurrency, cc.defaultConc)),
		)
		if err != nil {
			panic(fmt.Sprintf("build %s consumer: %v", cc.label, err))
		}
		sqsConsumers = append(sqsConsumers, cons)
		log.Info(cc.label+" consumer wired", map[string]interface{}{"queue_url": url})
	}
	for _, cons := range sqsConsumers {
		cons := cons
		go func() {
			if err := cons.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("SQS consumer stopped", map[string]interface{}{"error": err.Error()})
			}
		}()
	}

	// ── 9b. DB-state gauge exporters (§11.2) ──────────────────────────────
	// realm_active_gauge / realm_config_pending_gauge / template_drift are
	// counts of current table state, so no request or reconciler path can
	// maintain them. Refreshed here from the BYPASSRLS sysPool, matching
	// the sibling iam-org-membership exporter goroutines.
	runBusinessExporters(ctx, pgadapter.NewGaugeRepository(sysPool), log)

	// ── 10. Router ────────────────────────────────────────────────────────
	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: cfg,
		Docs: httpadapter.DocsConfig{
			Environment: appEnv,
			Enabled:     envOr("DOCS_ENABLED", "false") == "true",
			AuthToken:   os.Getenv("DOCS_AUTH_TOKEN"),
		},
		Handlers: internalH,
		Postgres: pingerFunc(func(ctx context.Context) error {
			if hs := pool.Health(ctx); !hs.Healthy {
				return fmt.Errorf("database not healthy")
			}
			return nil
		}),
		SysPostgres: pingerFunc(func(ctx context.Context) error {
			if hs := sysPool.Health(ctx); !hs.Healthy {
				return fmt.Errorf("sys database not healthy")
			}
			return nil
		}),
		Outbox: pingerFunc(func(context.Context) error {
			select {
			case <-outboxRunner.Ready():
				return nil
			default:
				return fmt.Errorf("outbox initializing")
			}
		}),
		Keycloak: pingerFunc(keycloakAdmin.Ready),
	})

	// ── 11. Graceful shutdown ─────────────────────────────────────────────
	srv := &http.Server{
		Addr:         ":" + envOr("APP_PORT", "8080"),
		Handler:      router.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 35 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              ":" + envOr("METRICS_PORT", "9090"),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("service starting", map[string]interface{}{
		"service": cfg.ServiceName, "version": cfg.BuildVersion, "env": appEnv, "addr": srv.Addr,
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", map[string]interface{}{"error": err.Error()})
		}
	}()
	go func() {
		log.Info("metrics server starting", map[string]interface{}{"addr": metricsServer.Addr})
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server error", map[string]interface{}{"error": err.Error()})
		}
	}()

	<-quit
	log.Info("shutdown signal received — draining", nil)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error", map[string]interface{}{"error": err.Error()})
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Error("metrics server shutdown error", map[string]interface{}{"error": err.Error()})
	}
	if err := outboxRunner.Stop(); err != nil {
		log.Error("outbox runner drain error", map[string]interface{}{"error": err.Error()})
	}
	for _, cons := range sqsConsumers {
		if err := cons.Stop(); err != nil {
			log.Error("SQS consumer stop error", map[string]interface{}{"error": err.Error()})
		}
	}
	cancelBackground()
	// DrainAndClose waits for in-flight WithConn/RunInTx callbacks then
	// force-closes — pgcommon's documented graceful path. defer Close()
	// above remains a safety net for panic/early-return and is idempotent
	// after DrainAndClose returns (must not run concurrently).
	if err := pool.DrainAndClose(shutdownCtx); err != nil {
		log.Error("app pool drain error", map[string]interface{}{"error": err.Error()})
	}
	if err := sysPool.DrainAndClose(shutdownCtx); err != nil {
		log.Error("sysPool drain error", map[string]interface{}{"error": err.Error()})
	}
	shutdownTracing()
	if err := gincommon.Shutdown(log); err != nil {
		log.Error("logger/tracer flush error", map[string]interface{}{"error": err.Error()})
	}
}

// pingerFunc adapts a plain func to httpadapter.Pinger.
type pingerFunc func(context.Context) error

func (f pingerFunc) Health(ctx context.Context) error { return f(ctx) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// isDevEnv reports whether appEnv is one of the local/CI-only values that
// permit a hard dependency's safe placeholder default (§12.1 fail-fast).
func isDevEnv(appEnv string) bool {
	return appEnv == "dev" || appEnv == "development" || appEnv == "local" || appEnv == "test"
}

// mustEnv panics with a descriptive message if key is empty outside
// APP_ENV=dev — used for hard dependencies (Keycloak, Org & Membership)
// with no safe placeholder default (§12.1 fail-fast).
func mustEnv(key, appEnv string) string {
	v := os.Getenv(key)
	if v == "" {
		if isDevEnv(appEnv) {
			return "http://localhost:8081"
		}
		panic(fmt.Sprintf("startup aborted — required env var %s is not set", key))
	}
	return v
}

// devDefaultValues maps a required env var to the literal placeholder
// .env-example ships for local development. validateRequiredEnv rejects
// any of these surviving into a non-dev APP_ENV unchanged — the failure
// mode this catches is a dev .env pasted into a staging/production secret
// verbatim: every other check here only looks for *emptiness*, which such
// a paste would never trigger, so the service would start and fail
// confusingly at first real Keycloak/O&M call instead of at boot.
var devDefaultValues = map[string]string{
	"KEYCLOAK_BASE_URL":            "http://localhost:8091",
	"KEYCLOAK_ADMIN_CLIENT_SECRET": "dev-secret-change-me",
	"ORG_MEMBERSHIP_BASE_URL":      "http://localhost:8082",
}

// validateRequiredEnv panics with every missing/misconfigured required env
// var listed at once (fail-fast, §12.1).
func validateRequiredEnv(appEnv string) {
	type req struct {
		key    string
		devOK  bool
		reason string
	}
	reqs := []req{
		{"KEYCLOAK_BASE_URL", true, "Keycloak Admin API base URL — RP-INV-1's sole seam"},
		{"KEYCLOAK_ADMIN_CLIENT_SECRET", true, "Keycloak admin service-account credential (OpenBao via ESO in prod)"},
		{"ORG_MEMBERSHIP_BASE_URL", true, "Org & Membership base URL — I-8 residual-MFA check"},
		{"SNS_TOPIC_TENANT_ARN", true, "iam-tenant-events topic ARN; events queue in outbox but never publish without it"},
		{"GLUE_REGISTRY_TENANT_NAME", true, "iam-tenant-events Glue registry name; NoopCodec (plain JSON) used without it"},
	}
	var missing []string
	for _, r := range reqs {
		v := os.Getenv(r.key)
		if v == "" {
			if r.devOK && isDevEnv(appEnv) {
				continue
			}
			missing = append(missing, r.key+": "+r.reason)
			continue
		}
		if !isDevEnv(appEnv) {
			if def, ok := devDefaultValues[r.key]; ok && v == def {
				missing = append(missing, r.key+": holds the local-dev placeholder value outside APP_ENV=dev — set a real value")
			}
		}
	}
	if os.Getenv("DATABASE_URL") == "" &&
		(os.Getenv("PG_HOST") == "" || os.Getenv("PG_USER") == "" || os.Getenv("PG_PASSWORD") == "") {
		missing = append(missing, "DATABASE_URL (or PG_HOST + PG_USER + PG_PASSWORD): PostgreSQL connection required")
	}
	if os.Getenv("MIGRATION_DATABASE_URL") == "" && os.Getenv("PG_BOUNCER_MODE") == "true" && os.Getenv("DATABASE_URL") == "" {
		missing = append(missing, "MIGRATION_DATABASE_URL: required when PG_BOUNCER_MODE=true — migrations must bypass PgBouncer")
	}
	if len(missing) > 0 {
		msg := "startup aborted — required env vars missing or misconfigured:\n"
		for _, m := range missing {
			msg += "  • " + m + "\n"
		}
		panic(msg)
	}
}
