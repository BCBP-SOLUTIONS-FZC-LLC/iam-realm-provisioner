package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// clearRequiredEnv unsets every key validateRequiredEnv/mustEnv consult, so
// each test starts from a clean slate regardless of the host's own
// environment or a sibling test's t.Setenv calls (t.Setenv restores on
// cleanup, but a key never set by this test could still leak in from the
// real process environment).
func clearRequiredEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"KEYCLOAK_BASE_URL", "KEYCLOAK_ADMIN_CLIENT_SECRET", "ORG_MEMBERSHIP_BASE_URL", "TOKEN_SERVICE_BASE_URL",
		"SNS_TOPIC_TENANT_ARN", "GLUE_REGISTRY_TENANT_NAME",
		"DATABASE_URL", "PG_HOST", "PG_USER", "PG_PASSWORD",
		"MIGRATION_DATABASE_URL", "PG_BOUNCER_MODE",
	} {
		t.Setenv(k, "")
	}
}

func setAllRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KEYCLOAK_BASE_URL", "https://keycloak.iam.svc.cluster.local")
	t.Setenv("KEYCLOAK_ADMIN_CLIENT_SECRET", "a-real-secret")
	t.Setenv("ORG_MEMBERSHIP_BASE_URL", "https://org-membership.iam.svc.cluster.local")
	t.Setenv("TOKEN_SERVICE_BASE_URL", "https://token-service.iam.svc.cluster.local")
	t.Setenv("SNS_TOPIC_TENANT_ARN", "arn:aws:sns:ap-south-1:111111111111:iam-tenant-events")
	t.Setenv("GLUE_REGISTRY_TENANT_NAME", "iam-tenant-events")
	t.Setenv("DATABASE_URL", "postgres://user:pass@host/db")
}

func TestIsDevEnv(t *testing.T) {
	for _, v := range []string{"dev", "development", "local", "test"} {
		assert.True(t, isDevEnv(v), v)
	}
	for _, v := range []string{"staging", "production", "prod", ""} {
		assert.False(t, isDevEnv(v), v)
	}
}

func TestValidateRequiredEnv_AllSet_NoPanic(t *testing.T) {
	clearRequiredEnv(t)
	setAllRequiredEnv(t)
	assert.NotPanics(t, func() { validateRequiredEnv("production") })
}

func TestValidateRequiredEnv_MissingOutsideDev_Panics(t *testing.T) {
	clearRequiredEnv(t)
	setAllRequiredEnv(t)
	t.Setenv("KEYCLOAK_BASE_URL", "")
	assert.PanicsWithValue(t,
		"startup aborted — required env vars missing or misconfigured:\n  • KEYCLOAK_BASE_URL: Keycloak Admin API base URL — RP-INV-1's sole seam\n",
		func() { validateRequiredEnv("production") })
}

func TestValidateRequiredEnv_MissingInDev_NoPanic(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("SNS_TOPIC_TENANT_ARN", "arn:aws:sns:ap-south-1:111111111111:iam-tenant-events")
	t.Setenv("GLUE_REGISTRY_TENANT_NAME", "iam-tenant-events")
	t.Setenv("DATABASE_URL", "postgres://user:pass@host/db")
	// KEYCLOAK_BASE_URL / KEYCLOAK_ADMIN_CLIENT_SECRET / ORG_MEMBERSHIP_BASE_URL
	// left unset — all three are devOK.
	assert.NotPanics(t, func() { validateRequiredEnv("dev") })
}

// CRITICAL (§12.1 fail-fast): a dev .env's placeholder value pasted
// verbatim into a non-dev environment must be caught at startup, not left
// to fail confusingly at the first real Keycloak/O&M call — this is the
// gap .claude/operations.md documents as already covered ("or holds a
// dev-default value outside APP_ENV=dev") that this test locks in.
func TestValidateRequiredEnv_DevDefaultValueOutsideDev_Panics(t *testing.T) {
	for key, devVal := range devDefaultValues {
		t.Run(key, func(t *testing.T) {
			clearRequiredEnv(t)
			setAllRequiredEnv(t)
			t.Setenv(key, devVal)
			assert.Panics(t, func() { validateRequiredEnv("production") },
				"%s holding its dev-default value %q must fail fast outside APP_ENV=dev", key, devVal)
		})
	}
}

// The same placeholder values are expected and fine inside dev/test — this
// is exactly what .env-example ships and every local/CI run relies on.
func TestValidateRequiredEnv_DevDefaultValueInsideDev_NoPanic(t *testing.T) {
	clearRequiredEnv(t)
	t.Setenv("SNS_TOPIC_TENANT_ARN", "arn:aws:sns:ap-south-1:111111111111:iam-tenant-events")
	t.Setenv("GLUE_REGISTRY_TENANT_NAME", "iam-tenant-events")
	t.Setenv("DATABASE_URL", "postgres://user:pass@host/db")
	for key, devVal := range devDefaultValues {
		t.Setenv(key, devVal)
	}
	assert.NotPanics(t, func() { validateRequiredEnv("dev") })
}

func TestValidateRequiredEnv_DatabaseURLMissing_Panics(t *testing.T) {
	clearRequiredEnv(t)
	setAllRequiredEnv(t)
	t.Setenv("DATABASE_URL", "")
	assert.Panics(t, func() { validateRequiredEnv("production") })
}

func TestValidateRequiredEnv_PGBouncerWithoutMigrationURL_Panics(t *testing.T) {
	clearRequiredEnv(t)
	setAllRequiredEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "db.internal")
	t.Setenv("PG_USER", "app")
	t.Setenv("PG_PASSWORD", "secret")
	t.Setenv("PG_BOUNCER_MODE", "true")
	assert.Panics(t, func() { validateRequiredEnv("production") })
}

func TestMustEnv_SetValue_Returned(t *testing.T) {
	t.Setenv("MUST_ENV_TEST_KEY", "real-value")
	assert.Equal(t, "real-value", mustEnv("MUST_ENV_TEST_KEY", "production"))
}

func TestMustEnv_UnsetOutsideDev_Panics(t *testing.T) {
	t.Setenv("MUST_ENV_TEST_KEY", "")
	assert.Panics(t, func() { mustEnv("MUST_ENV_TEST_KEY", "production") })
}

func TestMustEnv_UnsetInDev_ReturnsFallback(t *testing.T) {
	t.Setenv("MUST_ENV_TEST_KEY", "")
	assert.Equal(t, "http://localhost:8081", mustEnv("MUST_ENV_TEST_KEY", "dev"))
}
