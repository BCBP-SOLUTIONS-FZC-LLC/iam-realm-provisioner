#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's naming rules
# (namespace hierarchy, _total/_seconds suffixes, required
# {domain, service, environment} const labels, the iam_realm_provisioner_
# service prefix, registry label vocabularies, prohibited high-cardinality
# labels, no removed pre-standard names) against every Prometheus collector
# this service actually registers.
#
# internal/adapter/outbound/metrics/metrics_test.go's
# TestNamingConvention_ConformsToPlatformObservabilityStandard gathers the
# real registry (gincommon's HTTP metrics, this service's Tier 3 metrics and
# the shared platform_dependency_request_seconds) and validates it with
# platform-gincommon's pkg/obsregistry — the central Platform Observability
# Registry — rather than parsing Go source text or keeping a local copy.
set -euo pipefail

cd "$(dirname "$0")/../.."

go test ./internal/adapter/outbound/metrics/... \
  -run 'TestNamingConvention_ConformsToPlatformObservabilityStandard' \
  -v
