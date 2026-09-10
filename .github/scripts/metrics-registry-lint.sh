#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's governance
# rules 11/12 ("services SHALL NOT invent new platform_*/domain-shared
# metrics unilaterally"; "require observability governance approval
# before adoption"): a platform_* metric whose PlatformRegistry status
# (internal/adapter/outbound/metrics/registry.go) is StatusProposed — i.e.
# submitted but not yet ratified — must never be the live query target of
# a Prometheus alert, recording rule, or SLO expression anywhere in this
# repo. Mentioning a Proposed metric name in a comment (documenting the
# shadow-emission/migration plan) is fine and expected; using it inside an
# `expr:` is not, until the registry entry's Status flips to
# StatusCanonical.
set -euo pipefail

cd "$(dirname "$0")/../.."

REGISTRY_GO="internal/adapter/outbound/metrics/registry.go"
RULE_FILES=(
  deploy/monitoring/app-alerts.yml
  deploy/monitoring/slo-rules.yml
  deploy/helm/templates/prometheusrule.yaml
)

if [ ! -f "$REGISTRY_GO" ]; then
  echo "OK: no $REGISTRY_GO in this repo — nothing to check"
  exit 0
fi

# registry.go's PlatformRegistry is a flat slice of struct literals, one
# field per line (gofmt-aligned) — a small state machine over Name:/
# Status: pairs is enough for this hand-authored file; this is a repo
# lint script, not a general Go parser.
proposed_names=$(awk '
  /Name:[ \t]*"/ { match($0, /"[^"]+"/); name = substr($0, RSTART+1, RLENGTH-2); next }
  /Status:[ \t]*StatusProposed/ { if (name != "") print name; name="" }
  /Status:[ \t]*StatusCanonical/ { name="" }
' "$REGISTRY_GO")

if [ -z "$proposed_names" ]; then
  echo "OK: no Proposed (unratified) platform_* metrics in $REGISTRY_GO"
  exit 0
fi

echo "Proposed (unratified) platform_* metrics per $REGISTRY_GO:"
echo "$proposed_names" | sed 's/^/  - /'
echo

fail=0
for name in $proposed_names; do
  for f in "${RULE_FILES[@]}"; do
    [ -f "$f" ] || continue
    # Exclude comment lines (grep -n prefixes "N:", so the real content
    # starts after the first ':' — a comment line's content, ignoring
    # leading whitespace, starts with '#').
    matches=$(grep -n "$name" "$f" 2>/dev/null | grep -vE '^[0-9]+:[[:space:]]*#' || true)
    [ -z "$matches" ] && continue
    # A live PromQL reference carries a selector/function context; a
    # bare prose mention that somehow escaped the comment filter would
    # not.
    live=$(echo "$matches" | grep -E '\{|_count|_bucket|rate\(|increase\(' || true)
    if [ -n "$live" ]; then
      echo "FAIL: $f references Proposed metric '$name' outside a comment:"
      echo "$live" | sed 's/^/  /'
      fail=1
    fi
  done
done

if [ "$fail" -ne 0 ]; then
  echo
  echo "A Proposed platform_* metric (Status: StatusProposed in $REGISTRY_GO) may not source a production alert, recording rule, or SLO expression (Enterprise Platform Observability Standard, rules 11/12). Either point the rule back at its authoritative Tier 3 metric, or flip the registry entry's Status to StatusCanonical only once real observability governance has ratified it — then migrate dashboards/alerts/recording rules/SLOs/HPA together per the standard's Backward Compatibility steps."
  exit 1
fi

echo "OK: no Proposed platform_* metric is used as a live query target in ${RULE_FILES[*]}"
