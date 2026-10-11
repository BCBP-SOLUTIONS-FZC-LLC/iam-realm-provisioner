package main

import (
	"os"
	"strings"
)

// observabilityDomain is the Platform Observability Registry domain every
// IAM service reports under (the domain const label on every metric).
const observabilityDomain = "iam"

// resolveAppEnv returns the one environment name this process uses for
// everything: metrics, tracing, logging and its own dev/prod checks. It uses
// the same precedence the platform libraries use on their own (APP_ENV, then
// ENVIRONMENT; trimmed and lowercased), defaulting to "dev". Passing the
// result explicitly to gincommon (Config.Environment / TracingConfig) keeps
// metrics and traces from resolving it separately and disagreeing.
func resolveAppEnv() string {
	for _, key := range []string{"APP_ENV", "ENVIRONMENT"} {
		if v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v != "" {
			return v
		}
	}
	return "dev"
}

// isDevEnv reports whether appEnv is one of the developer environments
// (local, dev, test) that permit a hard dependency's safe placeholder
// default (§12.1 fail-fast).
func isDevEnv(appEnv string) bool {
	switch appEnv {
	case "local", "dev", "test":
		return true
	}
	return false
}

// loggerEnv maps appEnv onto the value gincommon's logger understands.
// gincommon treats only "dev"/"local" as development, so "test" (dev-like
// here: CI and local test runs) would otherwise get production JSON logs.
func loggerEnv(appEnv string) string {
	if isDevEnv(appEnv) {
		return "dev"
	}
	return appEnv
}
