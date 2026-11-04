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
	"sync"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gin-gonic/gin"

	_ "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/docs/swagger"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/consumer"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/sqspipeline"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	keycloakadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/openbao"
	orgmembershipclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/orgmembership"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	s3adapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/s3"
	tokenserviceclient "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/tokenservice"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
)

// buildVersion is injected by -ldflags at build time (see Dockerfile / Makefile).
var buildVersion = "dev"

// requestTimeout is the hard per-request deadline (gincommon.Config.RequestTimeout).
const requestTimeout = 30 * time.Second

// Shutdown budget stages (see the shutdown sequence in main): HTTP and
// conversion drain share httpDrainTimeout; the pools get a fresh
// poolDrainTimeout after the outbox drains.
const (
	httpDrainTimeout = 30 * time.Second
	poolDrainTimeout = 10 * time.Second
)

func main() {
	appEnv := resolveAppEnv()
	if !isDevEnv(appEnv) {
		gin.SetMode(gin.ReleaseMode)
	}

	validateRequiredEnv(appEnv)

	// ── 1. Logger — Zap via platform-gincommon ────────────────────────────
	log, err := logger.NewLogger(loggerEnv(appEnv))
	if err != nil {
		panic("init logger: " + err.Error())
	}

	cfg := gincommon.Config{
		Logger:       log,
		ServiceName:  envOr("APP_NAME", "iam-realm-provisioner"),
		BuildVersion: envOr("BUILD_VERSION", buildVersion),
		// Observability identity (Enterprise Platform Observability
		// Standard): domain is fixed for every IAM service; OBSERVABILITY_DOMAIN
		// in the Helm values/compose/.env-example carries the same value.
		// Environment is APP_ENV (local|dev|test|staging|prod — gincommon
		// panics on anything else, e.g. "production").
		Domain:      envOr("OBSERVABILITY_DOMAIN", observabilityDomain),
		Environment: appEnv,
		// RequestTimeout appends gincommon's TimeoutMiddleware as the
		// innermost observability handler, so metrics, the access log and
		// the server span record the same 503 the client receives. As an
		// outer r.Use it ran before observability, which recorded 200.
		RequestTimeout: requestTimeout,
	}

	// ── 2. Tracing — always install gincommon's TracerProvider so in-process
	// spans get valid trace IDs even when OTEL_EXPORTER_OTLP_ENDPOINT is unset
	// (dev), with the SAME identity as metrics (service, version, domain,
	// environment). InitTracingFromEnv read domain/environment from the
	// environment on its own, so traces could disagree with metrics.
	// ObservabilityMiddlewares' EnsureInitTelemetry is then a no-op.
	shutdownTracing, err := gincommon.InitTracingWithConfig(gincommon.TracingConfig{
		ServiceName:  cfg.ServiceName,
		BuildVersion: cfg.BuildVersion,
		Domain:       cfg.Domain,
		Environment:  cfg.Environment,
		Logger:       log,
	})
	if err != nil {
		panic("init tracing: " + err.Error())
	}
	// ObservabilityMiddlewares is gincommon's public metrics-init API. Call
	// it here (before any collector registration or exporter goroutine) so
	// business / events / pgcommon metrics land on gincommon.MetricsRegisterer
	// with matching {domain, service, environment} const labels. NewRouter
	// applies the same middleware slice to the Gin engine; gincommon's
	// metrics init is sync.Once.
	_ = gincommon.ObservabilityMiddlewares(cfg)

	// Business metrics first: Register builds the shared
	// platform_dependency_request_seconds collector that events.InitMetrics
	// then reuses for its SNS/SQS/codec calls.
	metrics.Register()

	// platform-events outbox/publish/consume metrics and platform-pgcommon
	// query/pool metrics share gincommon's registerer and identity so a
	// single /metrics scrape (gincommon.MetricsHandler on METRICS_PORT) serves HTTP +
	// business + outbox + pg collectors together. pgmetrics must be
	// initialized before NewPool below: the pool registers its
	// platform_db_pool_* gauges only when metrics are already initialized.
	initLibraryMetrics(log)

	// ── 3. Database — pgcommon.ConfigFromEnv reads DATABASE_URL/PG_* directly
	// so pool sizing, PgBouncer mode, and DSN assembly have exactly one
	// implementation instead of a second one hand-rolled here. ────────────
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	// PG_STATEMENT_TIMEOUT is applied per transaction by pgcommon itself
	// (Config.StatementTimeout from ConfigFromEnv), never baked into the DSN.
	dsn := pgadapter.DSNFromEnv()
	// Migrations must bypass PgBouncer because the migration runner acquires
	// a pg_advisory_lock, which is session-scoped and breaks under
	// transaction pooling. MIGRATION_DATABASE_URL points directly at
	// Postgres; falls back to dsn when unset.
	migrationDSN := pgadapter.MigrationDSNFromEnv()

	pgCfg.DSN = dsn
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(log)
	// db.query spans export through the TracerProvider
	// gincommon.InitTracingWithConfig installed above — same OTLP pipeline as
	// HTTP spans from ObservabilityMiddlewares.
	pgCfg.Tracer = gincommon.NewSpanTracer(cfg.ServiceName)
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
	sysCfg.Tracer = gincommon.NewSpanTracer(cfg.ServiceName)
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

	// Schema: in Kubernetes the migrate Job (cmd/migrate, a Helm
	// pre-install/pre-upgrade hook) migrates once per release and every
	// replica runs with RUN_MIGRATIONS=false. Unset or true (local dev,
	// tests), the server migrates at startup itself. MigrateSchema keeps the
	// §19 order: outbox.ApplySchema before the domain migration that ALTERs
	// outbox_events.
	if runMigrationsAtStartup() {
		if err := pgadapter.MigrateSchema(ctx, migrationDSN, log); err != nil {
			panic(fmt.Sprintf("migrate: %v", err))
		}
	} else {
		log.Info("RUN_MIGRATIONS=false — schema is migrated by the migrate Job", nil)
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
	// dlqPublisher is platform-events' DLQ forwarder over the same client. It
	// resolves each queue's DLQ from its own RedrivePolicy (cached, re-read on
	// expiry) and is used both by the consumers' automatic forwarding
	// (WithDLQForwarding) and by sqspipeline's straight-to-DLQ rejects.
	dlqPublisher, err := events.NewSQSDLQPublisherWithClient(events.DLQConfig{
		ConsumerName: cfg.ServiceName,
		Logger:       log,
	}, sqsClient)
	if err != nil {
		panic(fmt.Sprintf("build DLQ publisher: %v", err))
	}
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
	snsCodec, err := buildGlueCodec(ctx, glueClient, os.Getenv("GLUE_REGISTRY_TENANT_NAME"), schemaNames)
	if err != nil {
		panic(fmt.Sprintf("init glue codec: %v", err))
	}
	snsPublisher, err := buildSNSPublisher(os.Getenv("SNS_TOPIC_TENANT_ARN"), snsCodec, log)
	if err != nil {
		panic(fmt.Sprintf("build sns publisher: %v", err))
	}

	// ── 6. Outbox runner ─────────────────────────────────────────────────
	// LoadOutbox + RunnerConfigFromEnv is the platform-events composition
	// contract (same as iam-org-membership). Helm / .env-example keep this
	// service's historical values (500ms poll, concurrency 4, 2s jitter, 10m
	// claim lease) via OUTBOX_* env; library defaults apply only when those
	// are unset.
	outboxEnv := eventcfg.LoadOutbox()
	eventcfg.LogWarningsTo(log, outboxEnv.Warnings)
	outboxRunner, err := outbox.NewRunner(eventcfg.RunnerConfigFromEnv(outboxEnv, pool, snsPublisher, log))
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
	// tender-web's exact redirect URIs/origins (HLD §11.3) and the emailed
	// /t/<slug>/ links share one base URL.
	webClient, err := keycloakadapter.WebClientConfigFromEnv()
	if err != nil {
		panic(fmt.Sprintf("web client config: %v", err))
	}
	keycloakAdmin := keycloakadapter.New(keycloakadapter.Config{
		BaseURL:        mustEnv("KEYCLOAK_BASE_URL", appEnv),
		AdminRealm:     envOr("KEYCLOAK_ADMIN_REALM", "master"),
		ClientID:       envOr("KEYCLOAK_ADMIN_CLIENT_ID", "iam-realm-provisioner"),
		SecretProvider: keycloakadapter.EnvSecretFn("KEYCLOAK_ADMIN_CLIENT_SECRET"),
		RateLimitRPS:   envInt("KEYCLOAK_RATE_LIMIT_RPS", 20),
		Timeout:        envDuration("KEYCLOAK_TIMEOUT_MS", 8*time.Second),
		Web:            webClient,
	}, log)

	openbaoClient := openbao.New(openbao.Config{
		Addr:              envOr("BAO_ADDR", "https://openbao.iam.svc.cluster.local:8200"),
		AuthRole:          envOr("BAO_AUTH_ROLE", "iam-realm-provisioner"),
		KVMount:           envOr("BAO_KV_MOUNT", "secret"),
		IdpPathPrefix:     envOr("BAO_IDP_PATH_PREFIX", "iam/realm-provisioner/idp"),
		KeycloakAdminPath: envOr("BAO_KEYCLOAK_ADMIN_PATH", "iam/realm-provisioner/keycloak-admin"),
	}, log)

	orgMembershipClient := orgmembershipclient.New(mustEnv("ORG_MEMBERSHIP_BASE_URL", appEnv), envDuration("ORG_MEMBERSHIP_TIMEOUT_MS", 5*time.Second), log)
	tokenServiceClient := tokenserviceclient.New(mustEnv("TOKEN_SERVICE_BASE_URL", appEnv), log)

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
	idempotencyRepo := pgadapter.NewIdempotencyRepository(pool).WithInProgressLease(envDuration("IDEMPOTENCY_IN_PROGRESS_LEASE", pgadapter.DefaultInProgressLease))
	pendingAuditRepo := pgadapter.NewPendingAuditEntryRepository(pool)

	// ── 8b. Core services ─────────────────────────────────────────────────
	sharedRealm := envOr("SHARED_TRIAL_REALM", "trial")
	// Cross-tenant reads (realm-name owner, shard counts) go through the
	// BYPASSRLS sysPool; the app pool would see only the request tenant.
	sysRealmRepo := pgadapter.NewTenantRealmRepository(sysPool)
	provisioningSvc := service.NewProvisioningService(
		tenantRealmRepo, idempotencyRepo, keycloakAdmin, tokenServiceClient, txRunner, pendingAuditRepo, sharedRealm, nil,
	).WithLogger(log).WithConvertTimeout(envDuration("CONVERT_TIMEOUT", service.DefaultConvertTimeout)).
		WithWebEntry(webClient.BaseURLs[0], webClient.ClientID).
		WithRealmOwners(sysRealmRepo).
		WithShardLoads(sysRealmRepo)
	lifecycleSvc := service.NewLifecycleService(
		tenantRealmRepo, idempotencyRepo, keycloakAdmin, txRunner, sharedRealm,
	).WithLogger(log).WithIdpConfigs(idpConfigRepo)
	mfaSvc := service.NewMfaService(tenantRealmRepo, keycloakAdmin, orgMembershipClient).WithLogger(log)
	idpSvc := service.NewIdpService(tenantRealmRepo, idpConfigRepo, idpValidationRepo, idempotencyRepo, openbaoClient, keycloakAdmin, txRunner, pendingAuditRepo).WithLogger(log)
	realmConfigSvc := service.NewRealmConfigService(tenantRealmRepo, reconRepo, keycloakAdmin).WithLogger(log)

	internalH := httpadapter.NewInternalHandler(provisioningSvc, lifecycleSvc, mfaSvc, idpSvc, realmConfigSvc)

	// ── 9. Inbound SQS consumers (§7.1) ──────────────────────────────────
	// processed_events dedup through platform-events' inbox (port.Inbox):
	// the claim and each consumer's projection write commit in one
	// transaction (IDEMP-1..4), matching iam-org-membership.
	inbox := pgadapter.NewInboxRepository(pool, outboxPublisher)
	membershipConsumer := consumeradapter.NewMembershipConsumer(mfaSvc, inbox, log)
	tenantConsumer := consumeradapter.NewTenantConsumer(tenantRealmRepo, inbox, log)
	billingConsumer := consumeradapter.NewBillingConsumer(tenantRealmRepo, keycloakAdmin, inbox, log)

	// LoadSQS supplies region / endpoint / long-poll / visibility /
	// max-receive from the library env contract; per-queue URL + concurrency
	// overlay SQS_QUEUE_URL / SQS_CONCURRENCY, which cannot serve three
	// inbound queues.
	sqsEnv := eventcfg.LoadSQS()
	eventcfg.LogWarningsTo(log, sqsEnv.Warnings)

	var sqsConsumers []events.Consumer
	consumerCfgs := []struct {
		envVar      string
		concurrency string
		defaultConc int
		label       string
		consumer    string // frozen §25 processed_events.consumer value
		handler     events.Handler
	}{
		{"SQS_MEMBERSHIP_REALM_QUEUE_URL", "SQS_MEMBERSHIP_REALM_CONCURRENCY", 4, "membership-realm-q", string(domain.ConsumerMembership), membershipConsumer.Handle},
		{"SQS_TENANT_REALM_QUEUE_URL", "SQS_TENANT_REALM_CONCURRENCY", 4, "tenant-realm-q", string(domain.ConsumerTenant), tenantConsumer.Handle},
		{"SQS_BILLING_REALM_QUEUE_URL", "SQS_BILLING_REALM_CONCURRENCY", 2, "billing-realm-q", string(domain.ConsumerBilling), billingConsumer.Handle},
	}
	for _, cc := range consumerCfgs {
		url := os.Getenv(cc.envVar)
		if url == "" {
			log.Warn(cc.label+" consumer disabled — queue URL unset", nil)
			continue
		}
		if err := sqspipeline.RequireDLQ(ctx, dlqPublisher, url, log); err != nil {
			panic(fmt.Sprintf("DLQ preflight: %v", err))
		}
		validator, err := consumedValidator(cc.consumer)
		if err != nil {
			panic(fmt.Sprintf("compile %s consumed schemas: %v", cc.label, err))
		}
		cons, err := sqspipeline.NewConsumer(
			sqspipeline.EnvForQueue(sqsEnv, url, envInt(cc.concurrency, cc.defaultConc)),
			sqsClient,
			sqspipeline.Handler(cc.consumer, cc.label, cc.handler, validator, dlqPublisher, log),
			dlqPublisher,
			log,
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

	// ── 9c. AL-5 audit-entry delivery (§10.6) ─────────────────────────────
	// Drains pending_audit_entries (TenantIdpConfigChanged) to Audit Log's
	// direct-write ingest, cross-tenant on the BYPASSRLS sysPool.
	startAuditDelivery(ctx, sysPool, log)

	// ── 9d. Shared trial realm bootstrap (§12.2, RP-INV-2) ────────────────
	startSharedRealmEnsure(ctx, keycloakAdmin, sharedRealm, log)

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
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if hs := pool.Health(pingCtx); !hs.Healthy {
				return fmt.Errorf("database not healthy")
			}
			return nil
		}),
		SysPostgres: pingerFunc(func(ctx context.Context) error {
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if hs := sysPool.Health(pingCtx); !hs.Healthy {
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
		Keycloak: pingerFunc(func(ctx context.Context) error {
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return keycloakAdmin.Ready(pingCtx)
		}),
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
	metricsMux.Handle("/metrics", gincommon.MetricsHandler())
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

	// Order, and the budget it must fit in terminationGracePeriodSeconds (75,
	// deploy/helm/values.yaml) — matching iam-org-membership:
	//   1. in parallel: HTTP Shutdown (≤ 30s, the request timeout), every SQS
	//      consumer's Stop (each ≤ SQS_DRAIN_TIMEOUT, 15s) and detached RP-3
	//      conversions (≤ the same 30s) — stop new work, drain in-flight;
	//   2. outbox.Stop (≤ OUTBOX_DRAIN_TIMEOUT, 30s) — publishes committed
	//      events, after the producers above so none can enqueue past it;
	//   3. cancelBackground — exporters and the AL-5 audit drainer stop;
	//   4. pool drains on their own budget (poolDrainTimeout, 10s).
	// max(30, 15, 30) + 30 + 10 = 70s, leaving a 5s buffer. A conversion cut
	// off here keeps its key in_progress until the ledger lease expires, then
	// a retry resumes it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancel()
	var drain sync.WaitGroup
	drain.Add(2)
	go func() {
		defer drain.Done()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("HTTP server shutdown error", map[string]interface{}{"error": err.Error()})
		}
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			log.Error("metrics server shutdown error", map[string]interface{}{"error": err.Error()})
		}
	}()
	go func() {
		defer drain.Done()
		if err := provisioningSvc.Drain(shutdownCtx); err != nil {
			log.Error("in-flight conversions did not finish before shutdown (keys resume after the ledger lease)", map[string]interface{}{"error": err.Error()})
		}
	}()
	for _, cons := range sqsConsumers {
		drain.Add(1)
		go func() {
			defer drain.Done()
			if err := cons.Stop(); err != nil {
				log.Error("SQS consumer stop error", map[string]interface{}{"error": err.Error()})
			}
		}()
	}
	drain.Wait()
	if err := outboxRunner.Stop(); err != nil {
		log.Error("outbox runner drain error", map[string]interface{}{"error": err.Error()})
	}
	cancelBackground()
	// DrainAndClose waits for in-flight WithConn/RunInTx callbacks then
	// force-closes — pgcommon's documented graceful path. It gets a fresh
	// budget: the drain context above is usually spent by now. defer Close()
	// above remains a safety net for panic/early-return and is idempotent
	// after DrainAndClose returns (must not run concurrently).
	poolCtx, cancelPool := context.WithTimeout(context.Background(), poolDrainTimeout)
	defer cancelPool()
	if err := pool.DrainAndClose(poolCtx); err != nil {
		log.Error("app pool drain error", map[string]interface{}{"error": err.Error()})
	}
	if err := sysPool.DrainAndClose(poolCtx); err != nil {
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

// initLibraryMetrics initializes platform-events' and platform-pgcommon's
// Tier 1 metrics on gincommon's registerer with exactly the identity
// gincommon resolved ({domain, service, environment}), so all three
// libraries label the same values. Registration warnings (a platform_*
// metric that clashed and was disabled) are logged; an invalid identity is
// fatal. Must run after ObservabilityMiddlewares and before NewPool.
func initLibraryMetrics(log interface {
	Warn(msg string, fields map[string]interface{})
}) {
	reg := gincommon.MetricsRegisterer()
	labels := gincommon.MetricsConstLabels()

	evWarnings, err := events.InitMetrics(events.MetricsIdentityFromLabels(labels), reg)
	if err != nil {
		panic("init platform-events metrics: " + err.Error())
	}
	for _, w := range evWarnings {
		log.Warn("platform-events metric registration warning", map[string]interface{}{"metric": w.Metric, "error": w.Error()})
	}

	pgWarnings, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(labels), reg)
	if err != nil {
		panic("init platform-pgcommon metrics: " + err.Error())
	}
	for _, w := range pgWarnings {
		log.Warn("platform-pgcommon metric registration warning", map[string]interface{}{"metric": w.Metric, "error": w.Error()})
	}
}

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
// defaults (URLs and one obviously-fake dev secret), not real hardcoded
// credentials; validateRequiredEnv's whole purpose is rejecting them
// outside dev.
//
//nolint:gosec // G101 false positive — these are non-secret placeholder
var devDefaultValues = map[string]string{
	"KEYCLOAK_BASE_URL":            "http://localhost:8091",
	"KEYCLOAK_ADMIN_CLIENT_SECRET": "dev-secret-change-me",
	"ORG_MEMBERSHIP_BASE_URL":      "http://localhost:8082",
	"TOKEN_SERVICE_BASE_URL":       "http://localhost:8083",
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
		{"TOKEN_SERVICE_BASE_URL", true, "Token Service base URL — TS-1/TS-4 mint/rotate handshake (§2.5, RP-17)"},
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
	if !isDevEnv(appEnv) && os.Getenv("SYSTEM_DATABASE_URL") == "" {
		missing = append(missing, "SYSTEM_DATABASE_URL: required in production — sysPool must use a BYPASSRLS role (realm_provisioner_migrator); without it cross-tenant reconciler queries return zero rows and RP-C3 offboarding is silently disabled")
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
