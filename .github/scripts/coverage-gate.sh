#!/usr/bin/env bash
# Enforces minimum total test coverage.
#
# 70% matched the sibling IAM services' starting baseline (iam-tender-acl,
# iam-delegation) — not an aspiration, a floor. Ratcheted to 90% once real
# suites landed across every package (postgres/keycloak testcontainer
# coverage, full handler/service/eventbus/consumer coverage) took the
# measured total to ~92.2%. A prior pass judged the remaining gap —
# composition-root wiring in cmd/server/main.go/cmd/reconciler/main.go's
# run() — unreachable without a live Postgres/Keycloak/AWS connection; it
# turned out to be reachable after all; signal.Notify intercepts
# SIGINT/SIGTERM before the OS default-terminate action, which makes a
# signal-handling, panic-on-failure main() safely testable in-process
# against a real testcontainers Postgres (see cmd/server/
# main_integration_test.go and cmd/reconciler/main_integration_test.go's own
# doc comments), closing most of that gap and taking the measured total to
# ~97.8%. Ratcheted to 95%; the buffer below that keeps testcontainer-run
# variance from flaking an otherwise-good PR. Ratchet this up over time as
# more tests are added; never lower it to make a failing PR pass.
set -euo pipefail

THRESHOLD="${COVERAGE_THRESHOLD:-95}"

test -f coverage.out || {
  echo "::error file=coverage.out,title=Coverage gate::coverage.out missing - run 'make test-ci' before the coverage gate"
  exit 1
}

echo "::group::Coverage report summary"
pct=$(go tool cover -func=coverage.out | tail -1 | awk '{print $3}' | tr -d '%')
echo "Total coverage: ${pct}%"
echo "::endgroup::"

# Emit before the gate check so the value is available even when coverage fails.
echo "pct=${pct}" >> "$GITHUB_OUTPUT"

gate=$(awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN { print (p+0 < t) ? "FAIL" : "OK" }')
if [ "${gate}" = "FAIL" ]; then
  echo "::error file=coverage.out,title=Coverage gate::Coverage is ${pct}% - below the ${THRESHOLD}% threshold. Run 'make cover-func' locally to identify gaps."
  exit 1
fi
