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
