//go:build integration

// main() (cmd/server's composition root, ~180 statements, previously
// entirely untested) blocks on an OS signal and calls panic() on any
// wiring failure — the two properties that make it look untestable at
// first glance. Neither is actually a blocker: signal.Notify intercepts
// SIGTERM before the OS's default terminate-the-process action, so
// sending this test's own process a real SIGTERM only ever reaches
// main()'s `quit` channel, never kills the test binary; and every
// external dependency main() wires other than Postgres (Keycloak, Org &
// Membership, OpenBao, SNS, Glue, SQS, S3, Token Service) is either
// constructed lazily (no network call at construction time) or skipped
// outright when its env var is unset (SNS_TOPIC_TENANT_ARN unset ->
// noopPublisher; GLUE_REGISTRY_TENANT_NAME unset -> NoopCodec;
// SQS_*_QUEUE_URL unset -> that consumer is never started) — exactly the
// same "safe to leave unconfigured in dev" contract validateRequiredEnv
// itself already documents for APP_ENV=dev/test. So a real Postgres
// testcontainer is the only real dependency this test needs to exercise
// nearly the whole function, start to graceful shutdown.
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/testcontainers/testcontainers-go"
)

// freePort asks the OS for a currently-unused TCP port. There is an
// inherent, unavoidable TOCTOU race between closing this probe listener
// and main() binding the same port, same as every other test in the Go
// ecosystem that uses this pattern — acceptable for a test, not for
// production port allocation.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func setServerTestEnv(t *testing.T, dsn, appPort, metricsPort string) {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	t.Setenv("APP_PORT", appPort)
	t.Setenv("METRICS_PORT", metricsPort)
	t.Setenv("DATABASE_URL", dsn)
	// SYSTEM_DATABASE_URL / MIGRATION_DATABASE_URL deliberately left unset —
	// both fall back to DATABASE_URL (SystemDSNFromEnv/MigrationDSNFromEnv's
	// own documented contract), which also exercises main.go's "sysPool
	// reuses app DSN" WARN log path.
	t.Setenv("SYSTEM_DATABASE_URL", "")
	t.Setenv("MIGRATION_DATABASE_URL", "")
	t.Setenv("PG_BOUNCER_MODE", "false")
	t.Setenv("KEYCLOAK_BASE_URL", "")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "")
	t.Setenv("ORG_MEMBERSHIP_BASE_URL", "")
	t.Setenv("TOKEN_SERVICE_BASE_URL", "")
	t.Setenv("SNS_TOPIC_TENANT_ARN", "")
	t.Setenv("GLUE_REGISTRY_TENANT_NAME", "")
	t.Setenv("SQS_MEMBERSHIP_REALM_QUEUE_URL", "")
	t.Setenv("SQS_TENANT_REALM_QUEUE_URL", "")
	t.Setenv("SQS_BILLING_REALM_QUEUE_URL", "")
	t.Setenv("AWS_REGION", "ap-south-1")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("DOCS_ENABLED", "false")
}

func TestMain_FullWiring_ServesHealthzThenShutsDownGracefullyOnSIGTERM(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("provisioner"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	appPort := freePort(t)
	metricsPort := freePort(t)
	setServerTestEnv(t, dsn, appPort, metricsPort)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("main() panicked: %v", r)
			}
		}()
		main() // applies its own migrations; blocks until SIGTERM, then shuts down and returns
	}()

	// Poll /healthz (pure liveness, no dependency check) rather than sleep
	// a fixed duration — main() does real migration work before it starts
	// listening, so a fixed sleep would be either flaky or wastefully long.
	healthzURL := "http://127.0.0.1:" + appPort + "/healthz"
	require.Eventually(t, func() bool {
		resp, err := http.Get(healthzURL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond, "main() must be serving /healthz once startup completes")

	// /readyz additionally checks Postgres/sysPostgres/outbox/Keycloak —
	// exercising router.go's real dependency-health wiring end to end.
	readyzResp, err := http.Get("http://127.0.0.1:" + appPort + "/readyz")
	require.NoError(t, err)
	defer readyzResp.Body.Close()
	assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, readyzResp.StatusCode,
		"/readyz must respond with a well-formed status either way (Keycloak's own Ready() will report unhealthy here, which is expected — no real Keycloak is running)")

	metricsResp, err := http.Get("http://127.0.0.1:" + metricsPort + "/metrics")
	require.NoError(t, err)
	defer metricsResp.Body.Close()
	assert.Equal(t, http.StatusOK, metricsResp.StatusCode, "/metrics must be served on its own dedicated port")

	// signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM) in main() means
	// this intercepts the signal before the OS's default terminate action —
	// it reaches main()'s own shutdown path, not this test process's death.
	proc, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, proc.Signal(os.Interrupt))

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("main() did not return within 30s of SIGINT — graceful shutdown likely hung")
	}
}
