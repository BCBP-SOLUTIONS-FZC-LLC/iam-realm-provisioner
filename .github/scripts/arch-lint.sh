#!/usr/bin/env bash
# Installs go-arch-lint if missing, then enforces .go-arch-lint.yml rules
# (§3.4 — internal/core/* imports no adapter/Gin/pgx/gocloak/AWS SDK).
set -euo pipefail

if ! command -v go-arch-lint >/dev/null; then
  go install github.com/fe3dback/go-arch-lint@v1.15.0
fi
go-arch-lint check --project-path .

# RP-INV-1: no gocloak import outside internal/adapter/outbound/keycloak.
echo "Checking RP-INV-1 (single Keycloak writer)..."
offenders=$(grep -rl "Nerzal/gocloak" --include="*.go" . 2>/dev/null | grep -v "^\./internal/adapter/outbound/keycloak/" | grep -v "_test.go" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: gocloak imported outside internal/adapter/outbound/keycloak (RP-INV-1 violation):"
  echo "$offenders"
  exit 1
fi
echo "OK: gocloak is imported only by internal/adapter/outbound/keycloak"

# RLS-6 (no session-scoped tenant GUC) is checked once, inline in
# validate-quality.yml's "Reject non-transaction-local SET app.tenant_id"
# step — matching the sibling IAM services' convention (org-membership's
# own arch-lint.sh does only the go-arch-lint check above, with its RLS-6
# gate likewise inline in validate-quality.yml, not duplicated here).
