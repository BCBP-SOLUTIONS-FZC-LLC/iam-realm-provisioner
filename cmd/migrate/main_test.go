package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoggerEnv_MapsDevLikeEnvironmentsToDev(t *testing.T) {
	for env, want := range map[string]string{"local": "dev", "dev": "dev", "test": "dev", "staging": "staging", "prod": "prod", " PROD ": "prod"} {
		t.Setenv("APP_ENV", env)
		t.Setenv("ENVIRONMENT", "")
		assert.Equal(t, want, loggerEnv(), env)
	}
}

func TestLoggerEnv_FallsBackToEnvironmentThenDev(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("ENVIRONMENT", "staging")
	assert.Equal(t, "staging", loggerEnv())

	t.Setenv("ENVIRONMENT", "")
	assert.Equal(t, "dev", loggerEnv())
}

// An unreachable database fails the run (exit 1) within MIGRATE_TIMEOUT
// rather than hanging the Helm hook.
func TestRun_UnreachableDatabase_ReturnsOne(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	t.Setenv("MIGRATE_TIMEOUT", "3s")
	assert.Equal(t, 1, run())
}

// No DSN at all is a configuration error, reported before any connection.
func TestRun_NoDSN_ReturnsOne(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("MIGRATION_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	assert.Equal(t, 1, run())
}

// A malformed MIGRATE_TIMEOUT is silently ignored and the default (10 min) is
// used instead. The run still fails at the DSN check (exit 1) rather than
// panicking, so the Helm hook gets a clean failure rather than an unhandled
// crash.
func TestRun_MalformedMigrateTimeout_NoPanic_ReturnsOne(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("MIGRATION_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MIGRATE_TIMEOUT", "not-a-duration")
	assert.Equal(t, 1, run(), "malformed MIGRATE_TIMEOUT must fall back to default and fail on missing DSN, not panic")
}

// A non-positive MIGRATE_TIMEOUT (negative value) is silently ignored and the
// default (10 min) is used instead — matching the envDuration convention used
// across the reconciler and server. The run still fails at the DSN check.
func TestRun_NonPositiveMigrateTimeout_NoPanic_ReturnsOne(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("MIGRATION_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MIGRATE_TIMEOUT", "-5s")
	assert.Equal(t, 1, run(), "non-positive MIGRATE_TIMEOUT must fall back to default and fail on missing DSN, not panic")
}
