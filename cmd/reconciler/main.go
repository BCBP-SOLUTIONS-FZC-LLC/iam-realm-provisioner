// Package main is the single-binary reconciler entry point (§13.1).
// Selects a job by --job flag or RECONCILER_JOB env var and dispatches to
// the matching handler. Each of the 6 CronJobs in
// deploy/helm/templates/cronjobs.yaml invokes this binary with a different
// --job value.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"go.opentelemetry.io/otel"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/cmd/reconciler/jobs"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	keycloakadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	orgmembershipadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/orgmembership"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	s3adapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/s3"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

var registry = map[string]jobs.Func{
	"trial-expiry":           jobs.TrialExpiry,
	"trial-hard-delete":      jobs.TrialHardDelete,
	"subscription-lapse":     jobs.SubscriptionLapse,
	"realm-config-reconcile": jobs.RealmConfigReconcile,
	"outbox-prune":           jobs.OutboxPrune,
	"processed-events-prune": jobs.ProcessedEventsPrune,
}

func main() {
	os.Exit(run())
}

// run holds all deferred cleanup so that every exit path — success or job
// failure — runs it via a normal return, rather than an os.Exit call that
// would skip it.
func run() int {
	var jobName string
	flag.StringVar(&jobName, "job", os.Getenv("RECONCILER_JOB"), "reconciler job code")
	flag.Parse()

	if jobName == "" {
		die("no job specified — pass --job=<name> or set RECONCILER_JOB")
	}
	fn, ok := registry[jobName]
	if !ok {
		die("unknown job %q — valid: %v", jobName, registryNames())
	}

	rawLog, err := logger.NewLogger(envOr("APP_ENV", "dev"))
	if err != nil {
		panic("init logger: " + err.Error())
	}
	log := port.NewSlogStyleLogger(rawLog)

	// Same TracerProvider as cmd/server so db.query / keycloak.* spans from
	// this job export through gincommon's OTLP pipeline when the collector
	// is set, and still get valid in-process trace IDs when it is not.
	shutdownTracing := gincommon.InitTracingFromEnv()
	defer shutdownTracing()

	log.Info("reconciler starting", "job", jobName)
	serviceName := envOr("APP_NAME", "iam-realm-provisioner-reconciler")

	// Prime gincommon's metrics-init API so Register() picks up
	// {service, version} const labels, matching cmd/server. There is no
	// /metrics listener on this CronJob; the collectors still have to exist
	// because the jobs increment them.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		Logger:       rawLog,
		ServiceName:  serviceName,
		BuildVersion: envOr("BUILD_VERSION", "dev"),
	})
	metrics.Register()

	timeout := 5 * time.Minute
	if s := os.Getenv("RECONCILER_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ctx, jobSpan := otel.Tracer(serviceName).Start(ctx, "reconciler."+jobName)
	defer jobSpan.End()

	queryTracer := pgadapter.NewOTelTracer(serviceName)

	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", "key", w.Key, "reason", w.Reason)
	}
	pgCfg.DSN = pgadapter.DSNFromEnv()
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(rawLog)
	pgCfg.Tracer = queryTracer
	pool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		die("connect to postgres: %v", err)
	}

	// DrainAndClose is pgcommon's graceful path (wait for in-flight
	// WithConn/RunInTx, then close). A CronJob is short-lived and
	// os.Exit(1) on job failure skips defers, so drain is also invoked
	// explicitly on those paths. sync.Once keeps DrainAndClose and a
	// later defer from running concurrently (pgcommon forbids that).
	var sysPool *pgcommon.Pool
	var drainOnce sync.Once
	drainPools := func() {
		drainOnce.Do(func() {
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancelDrain()
			if err := pool.DrainAndClose(drainCtx); err != nil {
				log.Error("app pool drain error", "error", err.Error())
			}
			if sysPool != nil {
				if err := sysPool.DrainAndClose(drainCtx); err != nil {
					log.Error("sysPool drain error", "error", err.Error())
				}
			}
		})
	}
	defer drainPools()

	// *pgcommon.Pool via SystemPoolConfig — same helper as cmd/server
	// (no GUCProvider, PGBouncerMode forced true, ConfigFromEnv pool
	// sizing). Tracer is wired separately so db.query spans export
	// through gincommon's TracerProvider.
	sysCfg := pgadapter.SystemPoolConfig(pgadapter.SystemDSNFromEnv(), rawLog)
	sysCfg.Tracer = queryTracer
	sysPool, err = pgcommon.NewPool(ctx, sysCfg)
	if err != nil {
		drainPools()
		die("connect sysPool: %v", err)
	}

	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		drainPools()
		die("init validating codec: %v", err)
	}
	outboxPublisher := eventbusadapter.New("iam-realm-provisioner-reconciler", enqueueCodec).WithLogger(rawLog)
	txRunner := pgadapter.NewTxRunner(sysPool, outboxPublisher)

	appEnv := envOr("APP_ENV", "dev")
	keycloakAdmin := keycloakadapter.New(keycloakadapter.Config{
		BaseURL:      mustEnv("KEYCLOAK_BASE_URL", appEnv),
		AdminRealm:   envOr("KEYCLOAK_ADMIN_REALM", "master"),
		ClientID:     envOr("KEYCLOAK_ADMIN_CLIENT_ID", "iam-realm-provisioner"),
		ClientSecret: mustEnv("KEYCLOAK_ADMIN_CLIENT_SECRET", appEnv),
		RateLimitRPS: envInt("KEYCLOAK_RATE_LIMIT_RPS", 20),
		Timeout:      envDuration("KEYCLOAK_TIMEOUT_MS", 8*time.Second),
	}, rawLog)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(envOr("AWS_REGION", "ap-south-1")))
	if err != nil {
		drainPools()
		die("load aws config: %v", err)
	}
	var s3Opts []func(*s3.Options)
	if ep := os.Getenv("AWS_ENDPOINT_URL"); ep != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) { o.UsePathStyle = true; o.BaseEndpoint = &ep })
	}
	objectStore := s3adapter.New(s3.NewFromConfig(awsCfg, s3Opts...), s3adapter.Config{
		Bucket: envOr("REALM_EXPORT_BUCKET", "iam-realm-exports"),
		KMSKey: envOr("REALM_EXPORT_KMS_KEY", "alias/iam-realm-export"),
	})

	// SubscriptionLapseReader (RP-C3's suspend-phase trigger, O&M's I-16) is
	// optional at the process level — only the subscription-lapse job needs
	// it, so an unset base URL degrades that one job (WARN + skip its
	// suspend phase, see jobs.SubscriptionLapse) rather than failing every
	// other reconciler job's dispatch.
	var subscriptionLapses port.SubscriptionLapseReader
	if omBaseURL := envOr("ORG_MEMBERSHIP_BASE_URL", ""); omBaseURL != "" {
		subscriptionLapses = orgmembershipadapter.New(omBaseURL, rawLog)
	}

	var outboxRunner *outbox.Runner
	// The reconciler's own outbox-prune job reuses the same Runner shape
	// as cmd/server, constructed with a noop publisher — pruning never
	// publishes.
	outboxRunner, err = outbox.NewRunner(outbox.Config{Pool: sysPool, Publisher: noopPublisher{}, Logger: rawLog})
	if err != nil {
		drainPools()
		die("create outbox runner: %v", err)
	}

	jctx := &jobs.Context{
		Pool:                   pool,
		SysPool:                sysPool,
		OutboxRunner:           outboxRunner,
		TxRunner:               txRunner,
		TenantRealms:           pgadapter.NewTenantRealmRepository(sysPool),
		Reconciliations:        pgadapter.NewReconciliationRepository(sysPool),
		Idempotency:            pgadapter.NewIdempotencyRepository(sysPool),
		ProcessedEvents:        pgadapter.NewProcessedEventsRepository(sysPool),
		Keycloak:               keycloakAdmin,
		ObjectStore:            objectStore,
		SubscriptionLapses:     subscriptionLapses,
		Logger:                 log,
		BatchLimit:             envInt("RECONCILER_BATCH_LIMIT", 100),
		TrialGraceDays:         envInt("TRIAL_GRACE_DAYS", 30),
		CancelledGraceDays:     envInt("CANCELLED_GRACE_DAYS", 30),
		RetentionDays:          envInt("RETENTION_DAYS", 90),
		OutboxRetentionDays:    envInt("OUTBOX_RETENTION_DAYS", 8),
		ProcessedEventsTTLDays: envInt("PROCESSED_EVENTS_TTL_DAYS", 8),
		// RP-C4 poison-pill cap (§8.10): at the */10min schedule, 20 attempts
		// is ~3.3 hours of dedicated retry before a permanently-broken
		// realm-config setting is given up on and surfaced for operator
		// attention instead of retrying forever.
		MaxReconciliationAttempts: envInt("RECONCILER_MAX_ATTEMPTS", 20),
	}

	res, err := fn(ctx, jctx)
	if err != nil {
		log.Error("reconciler job failed", "job", jobName, "error", err.Error())
		if shutdownErr := gincommon.Shutdown(rawLog); shutdownErr != nil {
			log.Error("logger/tracer flush error", "error", shutdownErr.Error())
		}
		return 1
	}
	log.Info("reconciler complete", "job", jobName,
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	if shutdownErr := gincommon.Shutdown(rawLog); shutdownErr != nil {
		log.Error("logger/tracer flush error", "error", shutdownErr.Error())
	}
	return 0
}

func registryNames() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	return names
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

func mustEnv(key, appEnv string) string {
	v := os.Getenv(key)
	if v == "" {
		if appEnv == "dev" || appEnv == "development" || appEnv == "local" || appEnv == "test" {
			return "http://localhost:8081"
		}
		die("startup aborted — required env var %s is not set", key)
	}
	return v
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
