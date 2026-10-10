#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's naming rules
# against every Prometheus collector this service actually registers:
#
#   - Tier 3 (iam_realm_provisioner_*) counters must end in _total,
#     histograms in _seconds.
#   - No Tier 3 metric may encode the service name in a domain-shared
#     (iam_*) or platform-shared (platform_*) position.
#   - This service owns no platform_* metrics — those are owned by
#     platform-events and platform-gincommon and registered by
#     events.InitMetrics / gincommon.ObservabilityMiddlewares. Any
#     platform_* metric appearing in our registry was put there by one
#     of those libraries (correct), or was accidentally duplicated here
#     (incorrect — caught by the no-service-name-in-platform-name rule).
#
# Unlike a static source-regex scan, this validates against collectors
# actually registered at test time — so a new metric added to metrics.go
# without following the standard fails here automatically.
set -euo pipefail

cd "$(dirname "$0")/../.."

go test ./internal/adapter/outbound/metrics/... \
  -run 'TestNamingConvention_ConformsToPlatformObservabilityStandard' \
  -v
