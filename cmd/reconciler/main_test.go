package main

import (
	"flag"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetFlags gives each run() call a fresh flag.CommandLine — flag.StringVar
// panics ("flag redefined") if the same flag name is registered twice
// against the global CommandLine, which run() does on every call.
func resetFlags(t *testing.T, args ...string) {
	t.Helper()
	oldCommandLine := flag.CommandLine
	oldArgs := os.Args
	flag.CommandLine = flag.NewFlagSet(args[0], flag.ContinueOnError)
	os.Args = args
	t.Cleanup(func() {
		flag.CommandLine = oldCommandLine
		os.Args = oldArgs
	})
}

// stubExit replaces osExit with a recorder for the duration of the test.
func stubExit(t *testing.T) *int {
	t.Helper()
	var code int
	called := false
	old := osExit
	osExit = func(c int) {
		if !called {
			code = c
			called = true
		}
	}
	t.Cleanup(func() { osExit = old })
	return &code
}

func TestRun_NoJobSpecified_Returns1(t *testing.T) {
	t.Setenv("RECONCILER_JOB", "")
	resetFlags(t, "reconciler")
	exitCode := stubExit(t)

	got := run()

	assert.Equal(t, 1, got)
	assert.Equal(t, 1, *exitCode)
}

func TestRun_UnknownJob_Returns1(t *testing.T) {
	resetFlags(t, "reconciler", "--job=not-a-real-job")
	exitCode := stubExit(t)

	got := run()

	assert.Equal(t, 1, got)
	assert.Equal(t, 1, *exitCode)
}

// An invalid ORPHAN_SIGNUP_AFTER aborts orphaned-signup-reap before any
// connection is opened: the job is destructive, so no silent fallback.
func TestRun_OrphanedSignupReap_InvalidAfter_Returns1(t *testing.T) {
	for _, raw := range []string{"48", "-1h", "30m"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("ORPHAN_SIGNUP_AFTER", raw)
			resetFlags(t, "reconciler", "--job=orphaned-signup-reap")
			exitCode := stubExit(t)

			got := run()

			assert.Equal(t, 1, got)
			assert.Equal(t, 1, *exitCode)
		})
	}
}

func TestParseOrphanSignupAfter(t *testing.T) {
	d, err := parseOrphanSignupAfter("")
	require.NoError(t, err)
	assert.Equal(t, 48*time.Hour, d, "unset means the 48h default")

	d, err = parseOrphanSignupAfter("72h")
	require.NoError(t, err)
	assert.Equal(t, 72*time.Hour, d)

	d, err = parseOrphanSignupAfter("1h")
	require.NoError(t, err)
	assert.Equal(t, time.Hour, d, "the floor itself is allowed")

	for _, raw := range []string{"48", "two days", "59m", "0s", "-48h"} {
		d, err = parseOrphanSignupAfter(raw)
		require.Error(t, err, raw)
		assert.Equal(t, 48*time.Hour, d, "%s: the default is still returned for jobs that ignore the setting", raw)
	}
}

func TestRegistryNames(t *testing.T) {
	names := registryNames()
	assert.Len(t, names, len(registry))
	assert.Contains(t, names, "trial-expiry")
	assert.Contains(t, names, "outbox-prune")
	assert.Contains(t, names, "orphaned-signup-reap")
}

func TestEnvOr(t *testing.T) {
	t.Setenv("REC_TEST_ENVOR", "")
	assert.Equal(t, "default", envOr("REC_TEST_ENVOR", "default"))
	t.Setenv("REC_TEST_ENVOR", "custom")
	assert.Equal(t, "custom", envOr("REC_TEST_ENVOR", "default"))
}

func TestEnvInt(t *testing.T) {
	t.Setenv("REC_TEST_ENVINT", "")
	assert.Equal(t, 5, envInt("REC_TEST_ENVINT", 5))
	t.Setenv("REC_TEST_ENVINT", "42")
	assert.Equal(t, 42, envInt("REC_TEST_ENVINT", 5))
	t.Setenv("REC_TEST_ENVINT", "not-a-number")
	assert.Equal(t, 5, envInt("REC_TEST_ENVINT", 5), "unparseable falls back to default")
	t.Setenv("REC_TEST_ENVINT", "-3")
	assert.Equal(t, 5, envInt("REC_TEST_ENVINT", 5), "non-positive falls back to default")
}

func TestEnvDuration(t *testing.T) {
	defaultDur := 10 * time.Second
	t.Setenv("REC_TEST_ENVDUR", "")
	assert.Equal(t, defaultDur, envDuration("REC_TEST_ENVDUR", defaultDur))
	t.Setenv("REC_TEST_ENVDUR", "30s")
	assert.Equal(t, 30*time.Second, envDuration("REC_TEST_ENVDUR", defaultDur))
	t.Setenv("REC_TEST_ENVDUR", "not-a-duration")
	assert.Equal(t, defaultDur, envDuration("REC_TEST_ENVDUR", defaultDur), "unparseable falls back to default")
	t.Setenv("REC_TEST_ENVDUR", "-5s")
	assert.Equal(t, defaultDur, envDuration("REC_TEST_ENVDUR", defaultDur), "non-positive falls back to default")
}

func TestMustEnv_DevPlaceholder(t *testing.T) {
	t.Setenv("REC_TEST_MUSTENV", "")
	for _, appEnv := range []string{"dev", "local", "test"} {
		assert.Equal(t, "http://localhost:8081", mustEnv("REC_TEST_MUSTENV", appEnv))
	}
}

func TestMustEnv_SetValue(t *testing.T) {
	t.Setenv("REC_TEST_MUSTENV", "https://real.example.com")
	assert.Equal(t, "https://real.example.com", mustEnv("REC_TEST_MUSTENV", "prod"))
}

func TestMustEnv_MissingOutsideDev_Dies(t *testing.T) {
	t.Setenv("REC_TEST_MUSTENV", "")
	exitCode := stubExit(t)

	_ = mustEnv("REC_TEST_MUSTENV", "prod")

	require.NotNil(t, exitCode)
	assert.Equal(t, 1, *exitCode)
}

func TestDie(t *testing.T) {
	exitCode := stubExit(t)
	die("boom %d", 42)
	require.NotNil(t, exitCode)
	assert.Equal(t, 1, *exitCode)
}
