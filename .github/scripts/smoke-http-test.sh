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
# queue URLs (dev-mode WARN + skip), and the outbox runner starts with a
# stub SNS ARN that never publishes during the test. Keycloak is not
# started: /readyz returns 503 as expected (covered by the e2e suite).
# The goal here is to prove the image boots, connects to Postgres, and
# routes a real API request through to the DB.
#
# Uses --network host so both containers share the host network stack.
# --network host is supported on GitHub-hosted Ubuntu runners.
set -euo pipefail

SERVER_NAME=iam-realm-provisioner-smoke
PG_NAME=iam-realm-provisioner-smoke-pg

cleanup() {
  echo "::group::Cleanup — server logs (last 120 lines)"
  docker logs "$SERVER_NAME" 2>&1 | tail -120 || true
  echo "::endgroup::"
  docker rm -f "$SERVER_NAME" "$PG_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ── 1. Postgres ──────────────────────────────────────────────────────────────
echo "::group::Start Postgres"
docker run -d --name "$PG_NAME" --network host \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=postgres \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -v "$(pwd)/scripts/init-db.sql:/docker-entrypoint-initdb.d/init-db.sql:ro" \
  postgres:16-alpine

echo "Waiting for Postgres to be ready..."
for i in $(seq 1 30); do
  if docker exec "$PG_NAME" pg_isready -U postgres -q 2>/dev/null; then
    echo "Postgres ready after ${i} probe(s)."
    break
  fi
  sleep 2
done
if ! docker exec "$PG_NAME" pg_isready -U postgres -q 2>/dev/null; then
  echo "::error::Postgres did not become ready within 60s"
  docker logs "$PG_NAME" | tail -30
  exit 1
fi

# Verify the provisioner database and roles were created by init-db.sql.
echo "Checking provisioner DB and roles..."
docker exec "$PG_NAME" psql -U postgres -c "\l provisioner" 2>&1 | grep -q "provisioner" \
  || { echo "::error::provisioner database not found — init-db.sql may have failed"; docker logs "$PG_NAME" | tail -50; exit 1; }
docker exec "$PG_NAME" psql -U postgres -c "\du realm_provisioner_app" 2>&1 | grep -q "realm_provisioner_app" \
  || { echo "::error::realm_provisioner_app role not found"; exit 1; }
echo "provisioner DB and roles confirmed."
echo "::endgroup::"

# ── 2. Boot the server ───────────────────────────────────────────────────────
echo "::group::Boot iam-realm-provisioner-ci-test (server binary)"
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
echo "::group::Wait for /healthz"
ready=0
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/healthz" 2>/dev/null) || true
  echo "  probe ${i}: /healthz → ${code:-<no response>}"
  if [ "$code" = "200" ]; then
    ready=1
    break
  fi
  sleep 2
done
if [ "$ready" -ne 1 ]; then
  echo "::error::Never got /healthz=200 within 60s"
  exit 1
fi
echo "/healthz returned 200."
echo "::endgroup::"

# ── 4. /readyz diagnostic (503 expected without Keycloak) ───────────────────
echo "::group::Diagnostic — /readyz (503 expected, Keycloak not running)"
readyz_resp=$(curl -s "http://localhost:8080/readyz" 2>/dev/null) || true
echo "  /readyz body: ${readyz_resp:-<empty>}"
echo "::endgroup::"

# ── 5. /metrics ─────────────────────────────────────────────────────────────
echo "::group::Real HTTP request — /metrics"
metrics_code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:9090/metrics" 2>/dev/null) || true
echo "  /metrics → ${metrics_code:-<no response>}"
if [ "$metrics_code" != "200" ]; then
  echo "::error::GET /metrics returned ${metrics_code:-<empty>}, expected 200"
  exit 1
fi
echo "/metrics returned 200."
echo "::endgroup::"

# ── 6. RP-11 — real DB-backed request ───────────────────────────────────────
echo "::group::Real HTTP request — RP-11 GET /api/v1/internal/tenants/:id/realm"
FAKE_ID="550e8400-e29b-41d4-a716-446655440001"
rp11_full=$(curl -s -w '\n%{http_code}' \
  -H "Content-Type: application/json" \
  -H "x-user-id: ${FAKE_ID}" \
  -H "x-tenant-id: ${FAKE_ID}" \
  -H "x-tenant-roles: iam-system" \
  "http://localhost:8080/api/v1/internal/tenants/${FAKE_ID}/realm" 2>/dev/null) || true

rp11_code=$(printf '%s' "$rp11_full" | tail -1)
rp11_body=$(printf '%s' "$rp11_full" | head -n -1)
echo "  RP-11 HTTP code: ${rp11_code:-<empty>}"
echo "  RP-11 body:      ${rp11_body:-<empty>}"

if [ "$rp11_code" != "404" ]; then
  echo "::error::RP-11 returned ${rp11_code:-<empty>}, expected 404 tenant_realm_not_found. Body: ${rp11_body}"
  exit 1
fi
if ! printf '%s' "$rp11_body" | grep -qF 'tenant_realm_not_found'; then
  echo "::error::RP-11 body does not contain tenant_realm_not_found. Body: ${rp11_body}"
  exit 1
fi
echo "RP-11 correctly returned 404 tenant_realm_not_found."
echo "::endgroup::"

{
  echo "### Real HTTP smoke test results"
  echo "- ✅ Postgres started and provisioner schema migrated"
  echo "- ✅ /healthz returns 200"
  echo "- ✅ /metrics returns 200 (Prometheus registry bootstrapped)"
  echo "- ✅ RP-11 GET /realm returns 404 tenant_realm_not_found (router → Postgres verified)"
  echo ""
  echo "> /readyz: ${readyz_resp:-<empty>} (503 expected — Keycloak not started)"
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
