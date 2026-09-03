//go:build e2e

// Package e2e_test is the black-box end-to-end suite (operations.md §14
// "E2E" bullet): real internal/adapter/inbound/http.Router (the exact
// NewRouter construction cmd/server/wiring.go/main.go builds for
// production), fronting a real Postgres testcontainer and a real Keycloak
// testcontainer, driven over real HTTP (httptest.NewServer). No fakes stand
// in for Postgres or Keycloak — the two adapters RP-INV-1/RLS-6 exist to
// protect. OpenBao and Org & Membership are the only two outbound
// dependencies faked (in-memory, package-local): neither scenario below
// needs a real OpenBao KV store or a real O&M service, and standing up
// either just to satisfy wiring would add container cost without adding
// coverage (see idp_config_test.go/README-equivalent commentary at each use
// site). No SNS/SQS/Glue/S3 — the outbox.Runner is never started, so events
// are asserted as rows landing in outbox_events, never as anything actually
// published; the three SQS consumers are out of scope entirely (nothing in
// this suite calls them).
//
// One Postgres + one Keycloak container are started once for the whole
// package (TestMain) — a fresh container per scenario would multiply an
// already-heavy Keycloak boot (§14.2 measured ~30-60s) by the number of
// tests for no isolation benefit, since every scenario already uses a
// fresh uuid-derived tenant id/realm/email and therefore cannot collide
// with any other scenario sharing the same containers. Tests are safe to
// run with t.Parallel().
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/inbound/http"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/eventbus"
	keycloakadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/keycloak"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ── package-level test environment, built once by TestMain ─────────────────

const (
	appRolePassword = "e2e-app-password-testonly"

	kcClientID     = "iam-realm-provisioner"
	kcClientSecret = "e2e-test-secret"
	sharedRealm    = "trial" // hardcoded in the keycloak adapter's CreateTrialUser (RP-INV-2)
)

var (
	testServer *httptest.Server
	httpClient = &http.Client{Timeout: 30 * time.Second}

	rawPG *dbseed.Pool // pgcommon-backed superuser pool — bypasses RLS, used only for direct-state assertions

	kcAdmin   *keycloakadapter.Admin
	kcBaseURL string
)

// noopLogger satisfies port.Logger with no-ops, keeping e2e output focused
// on test assertions rather than adapter debug/info noise (mirrors
// test/integration's noopLogger).
type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

var _ port.Logger = noopLogger{}

// pingerFunc adapts a plain func to httpadapter.Pinger (mirrors
// cmd/server/main.go's identical helper).
type pingerFunc func(context.Context) error

func (f pingerFunc) Health(ctx context.Context) error { return f(ctx) }

// fakeOpenBao is a minimal in-memory port.OpenBaoClient. IdP secrets never
// need to survive a process restart in this suite (no DR/rebuild scenario
// is exercised), so a real OpenBao container would add setup cost with no
// additional coverage — RP-6/RP-7's HTTP-visible behavior (§11.6 gate,
// Keycloak IdP create/update/delete, the local keycloak_idp_configs row)
// does not depend on which OpenBaoClient implementation backs it.
type fakeOpenBao struct {
	secrets map[string]map[string]any
}

func newFakeOpenBao() *fakeOpenBao { return &fakeOpenBao{secrets: map[string]map[string]any{}} }

func openBaoKey(tenantID uuid.UUID, alias string) string { return tenantID.String() + "/" + alias }

func (f *fakeOpenBao) WriteIdpSecret(_ context.Context, tenantID uuid.UUID, alias string, secret map[string]any) error {
	f.secrets[openBaoKey(tenantID, alias)] = secret
	return nil
}

func (f *fakeOpenBao) ReadIdpSecret(_ context.Context, tenantID uuid.UUID, alias string) (map[string]any, error) {
	if s, ok := f.secrets[openBaoKey(tenantID, alias)]; ok {
		return s, nil
	}
	return nil, domain.NewError(domain.ErrIdpConfigNotFound, "secret not found")
}

func (f *fakeOpenBao) DeleteIdpSecret(_ context.Context, tenantID uuid.UUID, alias string) error {
	delete(f.secrets, openBaoKey(tenantID, alias))
	return nil
}

var _ port.OpenBaoClient = (*fakeOpenBao)(nil)

// fakeOrgMembership is a minimal port.OrgMembershipClient. No scenario in
// this suite exercises RP-9 (MFA reset) or the membership-realm-q
// SyncRequiresMfa reconcile path that consults it, but MfaService is a
// required InternalHandler dependency (§3.1's Handlers struct), so it must
// be wired to something.
type fakeOrgMembership struct{}

func (fakeOrgMembership) HasResidualMFARequirement(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, nil
}

var _ port.OrgMembershipClient = fakeOrgMembership{}

func TestMain(m *testing.M) {
	code, err := runE2ESuite(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup failed:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runE2ESuite builds the shared Postgres + Keycloak containers and the
// production-wired router exactly once, runs the suite, and tears
// everything down — returning m.Run()'s exit code to TestMain.
func runE2ESuite(m *testing.M) (int, error) {
	ctx := context.Background()

	pgContainer, superDSN, err := startPostgresContainer(ctx)
	if err != nil {
		return 0, fmt.Errorf("start postgres container: %w", err)
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	rawPG, err = dbseed.New(ctx, superDSN)
	if err != nil {
		return 0, fmt.Errorf("connect superuser pool: %w", err)
	}
	defer rawPG.Close()

	// Roles must exist BEFORE migrations run — the domain migration's GRANT
	// statements are conditional on realm_provisioner_app already existing
	// (mirrors test/postgres/rls_test.go's setupTestDB ordering).
	if _, err := rawPG.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE realm_provisioner_app LOGIN PASSWORD '%s' NOBYPASSRLS`, appRolePassword)); err != nil {
		return 0, fmt.Errorf("create realm_provisioner_app role: %w", err)
	}

	// outbox.ApplySchema FIRST (creates outbox_events with a JSONB payload
	// column) — the domain migration ALTERs that column to TEXT (§19).
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}); err != nil {
		return 0, fmt.Errorf("apply outbox schema: %w", err)
	}
	if err := pgadapter.RunMigrations(ctx, superDSN); err != nil {
		return 0, fmt.Errorf("run domain migrations: %w", err)
	}

	appDSN := appRoleDSN(rawPG, appRolePassword)
	appPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false, // testcontainer talks to Postgres directly
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	if err != nil {
		return 0, fmt.Errorf("connect app pool: %w", err)
	}
	defer appPool.Close()

	kcContainer, err := startKeycloakContainer(ctx)
	if err != nil {
		return 0, fmt.Errorf("start keycloak container: %w", err)
	}
	defer func() { _ = kcContainer.Terminate(ctx) }()

	kcBaseURL, err = keycloakBaseURL(ctx, kcContainer)
	if err != nil {
		return 0, fmt.Errorf("resolve keycloak base url: %w", err)
	}
	if err := bootstrapKeycloakServiceAccount(ctx, kcContainer); err != nil {
		return 0, fmt.Errorf("bootstrap keycloak service account: %w", err)
	}

	kcAdmin = keycloakadapter.New(keycloakadapter.Config{
		BaseURL: kcBaseURL, AdminRealm: "master",
		ClientID: kcClientID, ClientSecret: kcClientSecret,
		RateLimitRPS: 50, Timeout: 15 * time.Second,
	}, noopLogger{})
	if err := kcAdmin.Ready(ctx); err != nil {
		return 0, fmt.Errorf("keycloak admin not ready: %w", err)
	}
	// RP-1/RP-3 hardcode/derive the shared trial realm name "trial"
	// (RP-INV-2) — it must exist before any ProvisionTrial call.
	if err := kcAdmin.CreateRealmFromTemplate(ctx, sharedRealm, "v1"); err != nil {
		return 0, fmt.Errorf("create shared trial realm: %w", err)
	}

	// ── Production wiring (mirrors cmd/server/wiring.go/main.go §5-8) ──────
	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		return 0, fmt.Errorf("build validating codec: %w", err)
	}
	outboxPublisher := eventbusadapter.New("iam-realm-provisioner", enqueueCodec).WithLogger(noopLogger{})
	txRunner := pgadapter.NewTxRunner(appPool, outboxPublisher)

	tenantRealmRepo := pgadapter.NewTenantRealmRepository(appPool)
	idpConfigRepo := pgadapter.NewIdpConfigRepository(appPool)
	idpValidationRepo := pgadapter.NewIdpValidationRepository(appPool)
	reconRepo := pgadapter.NewReconciliationRepository(appPool)
	idempotencyRepo := pgadapter.NewIdempotencyRepository(appPool)

	provisioningSvc := service.NewProvisioningService(
		tenantRealmRepo, idempotencyRepo, kcAdmin, txRunner, sharedRealm, nil,
	).WithLogger(noopLogger{})
	lifecycleSvc := service.NewLifecycleService(
		tenantRealmRepo, idempotencyRepo, kcAdmin, txRunner, sharedRealm,
	).WithLogger(noopLogger{})
	mfaSvc := service.NewMfaService(tenantRealmRepo, kcAdmin, fakeOrgMembership{}).WithLogger(noopLogger{})
	idpSvc := service.NewIdpService(tenantRealmRepo, idpConfigRepo, idpValidationRepo, idempotencyRepo, newFakeOpenBao(), kcAdmin).WithLogger(noopLogger{})
	realmConfigSvc := service.NewRealmConfigService(tenantRealmRepo, reconRepo, kcAdmin).WithLogger(noopLogger{})

	internalH := httpadapter.NewInternalHandler(provisioningSvc, lifecycleSvc, mfaSvc, idpSvc, realmConfigSvc)

	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-realm-provisioner-e2e", Logger: noopLogger{}},
		Docs:      httpadapter.DocsConfig{Environment: "test"},
		Handlers:  internalH,
		Postgres: pingerFunc(func(ctx context.Context) error {
			if hs := appPool.Health(ctx); !hs.Healthy {
				return fmt.Errorf("database not healthy")
			}
			return nil
		}),
		Keycloak: pingerFunc(kcAdmin.Ready),
	})

	testServer = httptest.NewServer(router.Handler())
	defer testServer.Close()

	// NewRouter already primed ObservabilityMiddlewares; Register business
	// collectors so ProvisionTrial/etc. can increment them.
	metrics.Register()

	return m.Run(), nil
}

// startPostgresContainer mirrors test/postgres/rls_test.go's setupTestDB
// retry loop.
func startPostgresContainer(ctx context.Context) (*tcpostgres.PostgresContainer, string, error) {
	const maxAttempts = 3
	var pgContainer *tcpostgres.PostgresContainer
	var superDSN string
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pgContainer, err = tcpostgres.Run(ctx,
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
		if err == nil {
			superDSN, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
			if err == nil {
				return pgContainer, superDSN, nil
			}
			_ = pgContainer.Terminate(ctx)
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	return nil, "", err
}

func appRoleDSN(rawPool *dbseed.Pool, password string) string {
	cfg := rawPool.Config().ConnConfig
	return fmt.Sprintf("postgres://realm_provisioner_app:%s@%s:%d/%s?sslmode=disable",
		password, cfg.Host, cfg.Port, cfg.Database)
}

// startKeycloakContainer starts a Keycloak dev-mode container (mirrors
// test/integration/keycloak_test.go's helper of the same purpose — not
// importable across packages/build-tags, so reproduced here).
func startKeycloakContainer(ctx context.Context) (testcontainers.Container, error) {
	req := testcontainers.ContainerRequest{
		Image:        "quay.io/keycloak/keycloak:26.0",
		ExposedPorts: []string{"8080/tcp"},
		Env: map[string]string{
			"KEYCLOAK_ADMIN":          "admin",
			"KEYCLOAK_ADMIN_PASSWORD": "admin",
		},
		Cmd:        []string{"start-dev"},
		WaitingFor: wait.ForLog("Listening on: http://0.0.0.0:8080").WithStartupTimeout(120 * time.Second),
	}
	return testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
}

func keycloakBaseURL(ctx context.Context, container testcontainers.Container) (string, error) {
	mappedPort, err := container.MappedPort(ctx, "8080/tcp")
	if err != nil {
		return "", err
	}
	host, err := container.Host(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%s", host, mappedPort.Port()), nil
}

// bootstrapKeycloakServiceAccount provisions the "iam-realm-provisioner"
// master-realm service-account client with create-realm rights (mirrors
// scripts/keycloak-bootstrap.sh / test/integration's identical bootstrap).
func bootstrapKeycloakServiceAccount(ctx context.Context, container testcontainers.Container) error {
	script := fmt.Sprintf(`
/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin
/opt/keycloak/bin/kcadm.sh create clients -r master -s clientId=%s -s enabled=true -s publicClient=false -s serviceAccountsEnabled=true -s secret=%s
CLIENT_UUID=$(/opt/keycloak/bin/kcadm.sh get clients -r master -q clientId=%s --fields id --format csv --noquotes | tail -n1)
SA_USER_ID=$(/opt/keycloak/bin/kcadm.sh get clients/$CLIENT_UUID/service-account-user -r master --fields id --format csv --noquotes | tail -n1)
/opt/keycloak/bin/kcadm.sh add-roles -r master --uid $SA_USER_ID --rolename create-realm
`, kcClientID, kcClientSecret, kcClientID)

	exitCode, reader, err := container.Exec(ctx, []string{"bash", "-c", script})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(reader)
	if exitCode != 0 {
		return fmt.Errorf("keycloak bootstrap failed (exit %d): %s", exitCode, buf.String())
	}
	return nil
}

// rawKeycloakClient logs in as the same bootstrap service-account client
// kcAdmin uses, returning a raw gocloak client + token — for read-only
// assertions the KeycloakAdmin port deliberately doesn't expose (realm
// Enabled flag, IdentityProvider existence).
func rawKeycloakClient(ctx context.Context) (*gocloak.GoCloak, string, error) {
	client := gocloak.NewClient(kcBaseURL)
	jwt, err := client.LoginClient(ctx, kcClientID, kcClientSecret, "master")
	if err != nil {
		return nil, "", err
	}
	return client, jwt.AccessToken, nil
}

// ── HTTP helpers ─────────────────────────────────────────────────────────

// newTenantID returns a fresh, collision-free tenant id for one scenario.
func newTenantID() uuid.UUID { return uuid.New() }

// sysHeaders is a valid iam-system-principal caller scoped to tenantID —
// GUCBridgeMiddleware binds x-tenant-id straight into the RLS GUC
// (RLS-6), so every request acting on tenantID must carry it here, not an
// arbitrary id.
func sysHeaders(tenantID uuid.UUID) map[string]string {
	return map[string]string{
		"x-user-id":      "iam-system",
		"x-tenant-id":    tenantID.String(),
		"x-tenant-roles": "iam-system",
		"Content-Type":   "application/json",
	}
}

// operatorHeaders additionally carries platform_operator (RP-13/RP-14).
func operatorHeaders(tenantID uuid.UUID) map[string]string {
	h := sysHeaders(tenantID)
	h["x-tenant-roles"] = "iam-system,platform_operator"
	return h
}

// doRequest fires a real HTTP request at the shared httptest.Server and
// returns the raw response + body bytes.
func doRequest(t *testing.T, method, path string, headers map[string]string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, testServer.URL+path, rd)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, respBody
}

// doJSON marshals reqBody, fires the request, and decodes the response body
// into out (unless out is nil).
func doJSON(t *testing.T, method, path string, headers map[string]string, reqBody, out any) *http.Response {
	t.Helper()
	var raw []byte
	if reqBody != nil {
		var err error
		raw, err = json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
	}
	resp, respBody := doRequest(t, method, path, headers, raw)
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decode response body: %v, status=%d, body=%s", err, resp.StatusCode, respBody)
		}
	}
	return resp
}

// decodeErrorBody decodes body as httpadapter.ErrorResponse for assertions
// on the §17 error shape.
func decodeErrorBody(t *testing.T, method, path string, headers map[string]string, reqBody any) (*http.Response, httpadapter.ErrorResponse) {
	t.Helper()
	var er httpadapter.ErrorResponse
	resp := doJSON(t, method, path, headers, reqBody, &er)
	return resp, er
}

func newIdempotencyKey() string { return uuid.NewString() }
