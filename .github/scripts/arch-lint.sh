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

# RLS-6: no session-scoped `SET app.tenant_id` — only `SET LOCAL` via
# pgcommon.GUCSetFromContext.
echo "Checking RLS-6 (no session-scoped tenant GUC)..."
offenders=$(grep -rn "SET app\.tenant_id" --include="*.go" --include="*.sql" . 2>/dev/null | grep -v "SET LOCAL" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: found a non-LOCAL SET of app.tenant_id (RLS-6 violation):"
  echo "$offenders"
  exit 1
fi
echo "OK: no forbidden non-LOCAL SET app.tenant_id found"
