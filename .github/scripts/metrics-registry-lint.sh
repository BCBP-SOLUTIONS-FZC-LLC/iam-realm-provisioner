#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's registry rules on
# everything in this repo that queries metrics (alerts, recording rules,
# SLOs, the HPA and the prometheus-adapter rule), against the central
# Platform Observability Registry shipped in platform-gincommon
# (pkg/obsregistry) — there is no local copy of the registry any more:
#
#   1. No removed (pre-standard) metric name may appear in those files, even
#      in a comment: the platform libraries no longer emit them
#      (TestMonitoring_NoRemovedMetrics names each replacement).
#   2. Every alert that depends on a metric whose registry status is still
#      "proposed" (awaiting governance ratification) — directly or through a
#      recording rule — carries `metric_status: proposed`, and no other alert
#      does; an HPA scaling on one carries the
#      observability.platform/metric-status: proposed annotation
#      (TestMonitoring_ProposedMetricsAreLabelled).
#   3. Optionally, when METRICSLINT is set (e.g. METRICSLINT="go run
#      github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/cmd/metricslint"),
#      `metricslint refs` also scans deploy/, .github/, cmd/, internal/, the
#      runbooks and the top-level docs for removed or unregistered names. The
#      LLD and CHANGELOG are excluded: their revision history intentionally
#      names metrics that no longer exist.
set -euo pipefail

cd "$(dirname "$0")/../.."

go test ./internal/adapter/outbound/metrics/... \
  -run 'TestMonitoring_NoRemovedMetrics|TestMonitoring_ProposedMetricsAreLabelled' \
  -count=1 -v

if [ -n "${METRICSLINT:-}" ]; then
  # shellcheck disable=SC2086 # METRICSLINT may be a multi-word command.
  $METRICSLINT refs -ext .yml,.yaml,.json,.sh,.go,.md deploy .github cmd internal docs/runbook.md docs/architecture README.md ARCHITECTURE.md VERSIONING.md
fi
