#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's governance
# rules 11/12: this service MUST NOT use any platform_* metric as a live
# PromQL target (alert expr, recording-rule expr, SLO expr, HPA metric
# reference) because:
#
#   - platform_* metrics are owned by platform-events and platform-gincommon.
#   - This service's authoritative, alertable metrics are the Tier 3
#     iam_realm_provisioner_* family registered in
#     internal/adapter/outbound/metrics/metrics.go.
#   - Referencing a platform_* name in an alert/SLO before the Platform
#     Observability Registry has ratified it for that exact label schema
#     is prohibited (rules 11/12).
#
# The check is a simple grep: if any live PromQL expression in the rule
# files references a platform_* metric name, the build fails.
set -euo pipefail

cd "$(dirname "$0")/../.."

RULE_FILES=(
  deploy/monitoring/app-alerts.yml
  deploy/monitoring/slo-rules.yml
  deploy/helm/templates/prometheusrule.yaml
  deploy/helm/templates/hpa.yaml
  deploy/monitoring/prometheus-adapter-rule.yaml
)

fail=0
for f in "${RULE_FILES[@]}"; do
  [ -f "$f" ] || continue
  # Find non-comment lines that contain a platform_* name in a PromQL context
  # (a selector, _count/_bucket suffix, or a rate/increase call).
  matches=$(grep -n "platform_" "$f" 2>/dev/null | grep -vE '^[0-9]+:[[:space:]]*#' || true)
  [ -z "$matches" ] && continue
  live=$(echo "$matches" | grep -E '\{|_count|_bucket|rate\(|increase\(' || true)
  if [ -n "$live" ]; then
    echo "FAIL: $f references a platform_* metric in a live PromQL expression:"
    echo "$live" | sed 's/^/  /'
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo
  echo "This service's authoritative metrics are iam_realm_provisioner_* (Tier 3)."
  echo "platform_* metrics are owned by platform-events/platform-gincommon and must"
  echo "not source production alerts, recording rules, SLOs, or HPA references"
  echo "(Enterprise Platform Observability Standard, rules 11/12)."
  exit 1
fi

echo "OK: no platform_* metric is used as a live query target in ${RULE_FILES[*]}"
