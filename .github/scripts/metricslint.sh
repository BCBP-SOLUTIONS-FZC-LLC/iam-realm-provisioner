#!/usr/bin/env bash
# Runs platform-gincommon's metricslint (the Enterprise Platform Observability
# Standard's own linter, version pinned by go.mod) against this service.
#
#   1. check: a real /metrics scrape from the production registration order
#      (TestStandard_LibrariesShareRegistryCollectors, which records one sample
#      on every collector so every label set is validated).
#   2. refs:  alert rules, recording rules, dashboards and runbooks under
#      deploy/ and docs/ — legacy or unregistered metric names.
#   3. inventory drift: docs/observability/metric-registry.md must match
#      what .github/scripts/metrics-inventory.py renders from the registry
#      and the scrape. METRICS_INVENTORY_WRITE=1 rewrites it instead
#      (`make metrics-inventory`).
#
set -euo pipefail

ALLOW_PROPOSED_OWNERS="platform-gincommon,platform-events,platform-pgcommon"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

go build -o "$work/metricslint" github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/cmd/metricslint

METRICS_SCRAPE_OUT="$work/scrape.txt" \
  go test ./internal/adapter/outbound/metrics/ -count=1 -run '^TestStandard_LibrariesShareRegistryCollectors$' >/dev/null

set +e
"$work/metricslint" check -allow-proposed-owner "$ALLOW_PROPOSED_OWNERS" "$work/scrape.txt" >"$work/check.txt" 2>&1
check_rc=$?
set -e
cat "$work/check.txt"
if [ "$check_rc" -gt 1 ]; then
  echo "metricslint check: usage or I/O error (exit $check_rc)" >&2
  exit "$check_rc"
fi

if [ "$check_rc" -ne 0 ]; then
  echo "::error::metricslint check found violations of the observability standard (see above)."
  exit 1
fi
echo "metricslint check passed."

"$work/metricslint" refs deploy docs
echo "metricslint refs passed."

gincommon_dir=$(go list -m -f '{{.Dir}}' github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2)
gincommon_version=$(go list -m -f '{{.Version}}' github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2)
inventory=docs/observability/metric-registry.md
python3 .github/scripts/metrics-inventory.py "$gincommon_dir/internal/obsregistry/registry.json" \
  "$work/scrape.txt" "$gincommon_version" >"$work/metric-registry.md"
if [ "${METRICS_INVENTORY_WRITE:-}" = "1" ]; then
  cp "$work/metric-registry.md" "$inventory"
  echo "wrote $inventory"
elif ! diff -u "$inventory" "$work/metric-registry.md"; then
  echo "::error file=${inventory}::metric inventory is out of date; run make metrics-inventory"
  exit 1
else
  echo "metric inventory up to date."
fi
