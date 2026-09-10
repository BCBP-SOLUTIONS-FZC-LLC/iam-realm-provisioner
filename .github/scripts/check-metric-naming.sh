#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's naming rules
# (namespace hierarchy, _total/_seconds suffixes, required Tier 1 const
# labels) plus this service's own Platform Observability Registry
# compliance (a platform_* name must have a PlatformRegistry entry;
# registry.go, §1.7/§1.8) and label-vocabulary compliance, against every
# Prometheus collector this service actually registers.
#
# Unlike the sibling IAM services' check-metric-naming.sh (a static
# source-regex scan of internal/adapter/outbound/metrics/*.go), this
# service validates against collectors actually registered with
# Prometheus at test time: internal/adapter/outbound/metrics/
# metrics_test.go's TestNamingConvention_ConformsToPlatformObservabilityStandard
# gathers the real registry and checks it, rather than parsing Go source
# text. It also checks TestPlatformRegistry_EntriesAreWellFormed (guards
# registry.go itself). This service is the one that introduced the
# Proposed/Canonical registry-ratification model the sibling services'
# static scan has no equivalent for — a dynamic, registry-driven check is
# the only way to enforce it, and keeping one source of truth (the Go
# test) avoids a static rule-set and the real registry ever drifting
# apart.
set -euo pipefail

cd "$(dirname "$0")/../.."

go test ./internal/adapter/outbound/metrics/... \
  -run 'TestNamingConvention_ConformsToPlatformObservabilityStandard|TestPlatformRegistry_EntriesAreWellFormed' \
  -v
