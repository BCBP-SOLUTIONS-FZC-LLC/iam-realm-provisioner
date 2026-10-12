#!/usr/bin/env bash
# Real smoke test: boots the CI-built image (iam-realm-provisioner-ci-test)
# against a real Postgres container and issues real HTTP requests, proving
# the shipped image boots, self-migrates the provisioner schema, and serves
# traffic end-to-end.
#
# Uses docker run directly (not docker compose up) to avoid compose-file
# validation failures in CI — docker-compose.yml's go_private_token secret
# has no file: or external: source and fails validation when compose reads
# the full file, even when only postgres/floci are requested.
#
# Floci (SNS/SQS) is not started: SQS consumers are disabled via empty
# queue URLs (dev-mode WARN + skip, per Helm values.yaml comment), and the
# outbox runner starts with a stub SNS ARN that never publishes during the
# test. Keycloak is not started: /readyz returns 503 as expected (Keycloak
# coverage lives in the e2e suite). The goal here is to prove the image
# boots, connects to Postgres, and routes a real API request to the DB.
#
# Uses --network host so both containers share the host network stack —
# the app container reaches postgres on localhost:5432 exactly the way a
# locally-run binary does. --network host is supported on GitHub-hosted
# Ubuntu runners.
set -euo pipefail

SERVER_NAME=iam-realm-provisioner-smoke
PG_NAME=iam-realm-provisioner-smoke-pg

cleanup() {
  echo "::group::Cleanup — server logs"
  docker logs "$SERVER_NAME" 2>&1 | tail -120 || true
  docker rm -f "$SERVER_NAME" "$PG_NAME" >/dev/null 2>&1 || true
  echo "::endgroup::"
}
trap cleanup EXIT

# ── 1. Postgres ──────────────────────────────────────────────────────────────
echo "::group::Start Postgres"
# Mount scripts/init-db.sql so the postgres entrypoint creates the two roles
# (realm_provisioner_app, realm_provisioner_migrator) and the provisioner DB.
# --network host: postgres listens on host port 5432 directly.
docker run -d --name "$PG_NAME" --network host \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=postgres \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -v "$(pwd)/scripts/init-db.sql:/docker-entrypoint-initdb.d/init-db.sql:ro" \
  postgres:16-alpine

echo "waiting for Postgres..."
for _ in $(seq 1 30); do
  if docker exec "$PG_NAME" pg_isready -U postgres -q 2>/dev/null; then
    break
  fi
  sleep 2
done
if ! docker exec "$PG_NAME" pg_isready -U postgres -q 2>/dev/null; then
  echo "::error::Postgres did not become ready within 60s"
  docker logs "$PG_NAME" | tail -30
  exit 1
fi
echo "Postgres is ready."
echo "::endgroup::"

# ── 2. Boot the server ───────────────────────────────────────────────────────
echo "::group::Boot iam-realm-provisioner-ci-test (server binary)"
# Key choices:
#   APP_ENV=dev        — skips strict SYSTEM_DATABASE_URL panic in non-dev
#   PG_PORT=5432       — postgres is on the host network (docker run --network host)
#   SQS_*_QUEUE_URL="" — empty URL silently disables that consumer (WARN, documented)
#   GLUE_REGISTRY_TENANT_NAME unset — NoopCodec; no Glue calls at startup
#   SNS_TOPIC_TENANT_ARN set but endpoint unreachable — outbox runner starts,
#   publishes fail at run time (not startup), which is fine for a smoke test
#   KEYCLOAK_ADMIN_CLIENT_SECRET — any non-empty string; Keycloak not running
docker run -d --name "$SERVER_NAME" --network host \
  -e APP_ENV=dev \
  -e APP_PORT=8080 \
  -e METRICS_PORT=9090 \
  -e PG_HOST=localhost \
  -e PG_PORT=5432 \
  -e PG_USER=realm_provisioner_app \
  -e PG_PASSWORD=realm_provisioner_app_dev_password \
  -e PG_DBNAME=provisioner \
  -e PG_SSLMODE=disable \
  -e SYSTEM_DATABASE_URL="postgres://realm_provisioner_migrator:realm_provisioner_migrator_dev_password@localhost:5432/provisioner?sslmode=disable" \
  -e MIGRATION_DATABASE_URL="postgres://realm_provisioner_migrator:realm_provisioner_migrator_dev_password@localhost:5432/provisioner?sslmode=disable" \
  -e KEYCLOAK_BASE_URL="http://localhost:1" \
  -e KEYCLOAK_ADMIN_CLIENT_SECRET="ci-smoke-placeholder" \
  -e SNS_TOPIC_TENANT_ARN="arn:aws:sns:ap-south-1:000000000000:iam-tenant-events" \
  -e SQS_MEMBERSHIP_REALM_QUEUE_URL="" \
  -e SQS_TENANT_REALM_QUEUE_URL="" \
  -e SQS_BILLING_REALM_QUEUE_URL="" \
  -e AWS_REGION=ap-south-1 \
  -e AWS_ENDPOINT_URL="http://localhost:1" \
  -e AWS_ACCESS_KEY_ID=test \
  -e AWS_SECRET_ACCESS_KEY=test \
  -e OBSERVABILITY_DOMAIN=iam \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=none \
  iam-realm-provisioner-ci-test
echo "::endgroup::"

# ── 3. /healthz ─────────────────────────────────────────────────────────────
echo "::group::Wait for /healthz (liveness — no external dependencies)"
ready=0
for _ in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/healthz" 2>/dev/null) || true
  if [ "$code" = "200" ]; then
    ready=1
    break
  fi
  sleep 2
done
if [ "$ready" -ne 1 ]; then
  echo "::error::iam-realm-provisioner-ci-test never reported /healthz=200 within 60s — the shipped image did not boot."
  exit 1
fi
echo "/healthz returned 200."
echo "::endgroup::"

# ── 4. /metrics ─────────────────────────────────────────────────────────────
echo "::group::Real HTTP request — /metrics (Prometheus scrape endpoint)"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:9090/metrics" 2>/dev/null) || true
if [ "$code" != "200" ]; then
  echo "::error::GET /metrics returned ${code}, expected 200"
  exit 1
fi
echo "/metrics returned 200."
echo "::endgroup::"

# ── 5. RP-11 — real DB-backed request ───────────────────────────────────────
echo "::group::Real HTTP request — RP-11 GET /api/v1/internal/tenants/:id/realm"
# RequireSystemRole needs x-tenant-roles: iam-system.
# RequireTenantMatch needs x-tenant-id == path :id.
# A nonexistent tenant → 404 tenant_realm_not_found: exercises the full
# router → GUCBridgeMiddleware → LifecycleService → TenantRealmRepository
# → Postgres path against a real database.
FAKE_ID="00000000-0000-0000-0000-000000000001"
RESP=$(curl -s -w '\n%{http_code}' \
  -H "x-user-id: ${FAKE_ID}" \
  -H "x-tenant-id: ${FAKE_ID}" \
  -H "x-tenant-roles: iam-system" \
  "http://localhost:8080/api/v1/internal/tenants/${FAKE_ID}/realm" 2>/dev/null) || true
CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | head -n -1)
if [ "$CODE" != "404" ]; then
  echo "::error::GET RP-11 returned ${CODE}, expected 404 tenant_realm_not_found. Body: ${BODY}"
  exit 1
fi
if ! echo "$BODY" | grep -q '"code":"tenant_realm_not_found"'; then
  echo "::error::RP-11 response did not contain code:tenant_realm_not_found. Body: ${BODY}"
  exit 1
fi
echo "RP-11 correctly returned 404 tenant_realm_not_found for a nonexistent tenant."
echo "::endgroup::"

{
  echo "### Real HTTP smoke test results"
  echo "- ✅ Postgres started and provisioner schema migrated"
  echo "- ✅ /healthz returns 200 (liveness)"
  echo "- ✅ /metrics returns 200 (Prometheus registry bootstrapped)"
  echo "- ✅ RP-11 GET /realm returns 404 tenant_realm_not_found (router → service → Postgres verified)"
  echo ""
  echo "> /readyz returns 503 as expected — Keycloak is not started in this smoke environment."
  echo "> Keycloak + OpenBao integration is covered by \`make test-integration\` and the e2e suite."
} >> "$GITHUB_STEP_SUMMARY"
