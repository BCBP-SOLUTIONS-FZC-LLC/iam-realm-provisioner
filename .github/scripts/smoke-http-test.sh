#!/usr/bin/env bash
# Real smoke test: boots the CI-built image (iam-realm-provisioner-ci-test)
# against real Postgres + Floci (SNS/SQS) containers — the same services
# docker-compose.yml local development uses — and issues real HTTP requests,
# proving the shipped image boots, self-migrates, and serves traffic end-to-end.
#
# smoke-tests.sh (the companion script) only proves each binary exits non-zero
# on missing env vars. This gate proves the shipped image actually boots,
# connects to Postgres, applies schema migrations, serves the metrics port, and
# handles a real API request through the full router → service → repository →
# Postgres path — the regression class that only shows up at runtime.
#
# /readyz returns 503 in this environment because Keycloak is not started
# (Keycloak 26's Quarkus augmentation alone takes ~60s; that coverage is
# owned by test/e2e via testcontainers). /healthz is the liveness signal;
# /metrics proves the Prometheus endpoint is serving; RP-11 via the internal
# API proves the DB-backed path returns a typed domain error rather than a
# 500 or a connection panic.
#
# Uses --network host so the app container reaches docker-compose's host-mapped
# ports (5544 for Postgres, 4572 for Floci) exactly as a local `go run` binary
# does. --network host is supported on GitHub-hosted Ubuntu runners.
set -euo pipefail

CONTAINER_NAME=iam-realm-provisioner-smoke
# Start only the services the server actually needs to boot and handle a
# request; Keycloak and OpenBao are excluded intentionally — this test proves
# database connectivity and routing, not IdP integration.
COMPOSE_SERVICES=(postgres floci)

cleanup() {
  echo "::group::Cleanup — container logs"
  docker logs "$CONTAINER_NAME" 2>&1 | tail -120 || true
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  echo "::endgroup::"
}
trap cleanup EXIT

echo "::group::Start dependencies (postgres, floci)"
docker compose up -d "${COMPOSE_SERVICES[@]}"
# Wait for each service's own docker-compose healthcheck (pg_isready for
# Postgres; Glue get-schema for Floci, which proves init-floci.sh completed).
for svc in "${COMPOSE_SERVICES[@]}"; do
  cid=$(docker compose ps -q "$svc")
  echo "waiting for $svc to report healthy..."
  for _ in $(seq 1 40); do
    status=$(docker inspect --format='{{.State.Health.Status}}' "$cid" 2>/dev/null || echo "unknown")
    [ "$status" = "healthy" ] && break
    sleep 3
  done
  status=$(docker inspect --format='{{.State.Health.Status}}' "$cid" 2>/dev/null || echo "unknown")
  if [ "$status" != "healthy" ]; then
    echo "::error::$svc did not become healthy in time (status: $status)"
    docker compose logs "$svc" | tail -60
    exit 1
  fi
  echo "$svc is healthy."
done
echo "::endgroup::"

echo "::group::Boot iam-realm-provisioner-ci-test (server binary)"
# SQS queue URLs match init-floci.sh's provisioned names on Floci's host port
# 4572. SYSTEM_DATABASE_URL and MIGRATION_DATABASE_URL are set explicitly —
# the server and migrate-hook binary both require a BYPASSRLS-capable role.
# KEYCLOAK_BASE_URL points to an unreachable address so the server starts
# (the URL is validated non-empty, not actually dialled at boot) but /readyz
# correctly returns 503 for Keycloak (expected — see script header comment).
# RUN_MIGRATIONS=true (the default) so the image applies the domain schema
# on first boot via the compose run, matching the Helm pre-install hook flow.
docker run -d --name "$CONTAINER_NAME" --network host \
  -e APP_ENV=dev \
  -e APP_PORT=8080 \
  -e METRICS_PORT=9090 \
  -e PG_HOST=localhost \
  -e PG_PORT=5544 \
  -e PG_USER=realm_provisioner_app \
  -e PG_PASSWORD=realm_provisioner_app_dev_password \
  -e PG_DBNAME=provisioner \
  -e PG_SSLMODE=disable \
  -e SYSTEM_DATABASE_URL="postgres://realm_provisioner_migrator:realm_provisioner_migrator_dev_password@localhost:5544/provisioner?sslmode=disable" \
  -e MIGRATION_DATABASE_URL="postgres://realm_provisioner_migrator:realm_provisioner_migrator_dev_password@localhost:5544/provisioner?sslmode=disable" \
  -e KEYCLOAK_BASE_URL="http://localhost:1" \
  -e KEYCLOAK_ADMIN_CLIENT_SECRET="ci-smoke-placeholder" \
  -e SNS_TOPIC_TENANT_ARN="arn:aws:sns:ap-south-1:000000000000:iam-tenant-events" \
  -e SQS_MEMBERSHIP_REALM_QUEUE_URL="http://localhost:4572/000000000000/membership-realm-q" \
  -e SQS_TENANT_REALM_QUEUE_URL="http://localhost:4572/000000000000/tenant-realm-q" \
  -e SQS_BILLING_REALM_QUEUE_URL="http://localhost:4572/000000000000/billing-realm-q" \
  -e SQS_DLQ_URL="http://localhost:4572/000000000000/membership-realm-q-dlq" \
  -e AWS_REGION=ap-south-1 \
  -e AWS_ENDPOINT_URL="http://localhost:4572" \
  -e AWS_ACCESS_KEY_ID=test \
  -e AWS_SECRET_ACCESS_KEY=test \
  -e OBSERVABILITY_DOMAIN=iam \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=none \
  iam-realm-provisioner-ci-test
echo "::endgroup::"

echo "::group::Wait for /healthz (liveness — no external dependencies)"
ready=0
for _ in $(seq 1 30); do
  if code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/healthz" 2>/dev/null) && [ "$code" = "200" ]; then
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

echo "::group::Real HTTP request — /metrics (Prometheus scrape endpoint)"
# /metrics is served on METRICS_PORT=9090, isolated from the mesh-internal API.
# A 200 here proves the Prometheus registry was bootstrapped successfully.
code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:9090/metrics")
if [ "$code" != "200" ]; then
  echo "::error::GET /metrics returned ${code}, expected 200"
  exit 1
fi
echo "/metrics returned 200."
echo "::endgroup::"

echo "::group::Real HTTP request — RP-11 (GET /api/v1/internal/tenants/:id/realm)"
# RP-11 requires RequireSystemRole (x-tenant-roles: iam-system) and
# RequireTenantMatch (x-tenant-id must equal the path :id). A non-existent
# tenant UUID must return 404 tenant_realm_not_found — exercising the full
# router → GUCBridgeMiddleware → LifecycleService → TenantRealmRepository
# → Postgres path against a real database, never a mocked one. This is the
# exact regression class (DB-backed 404 vs connection panic) that shipping
# a broken image would surface only at runtime.
FAKE_ID="00000000-0000-0000-0000-000000000001"
RESP=$(curl -s -w '\n%{http_code}' \
  -H "x-user-id: ${FAKE_ID}" \
  -H "x-tenant-id: ${FAKE_ID}" \
  -H "x-tenant-roles: iam-system" \
  "http://localhost:8080/api/v1/internal/tenants/${FAKE_ID}/realm")
CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | head -n -1)
if [ "$CODE" != "404" ]; then
  echo "::error::GET RP-11 returned ${CODE}, expected 404 tenant_realm_not_found for a nonexistent tenant. Body: ${BODY}"
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
  echo "- ✅ Image boots against real Postgres and Floci, self-migrates the provisioner schema"
  echo "- ✅ /healthz returns 200 (liveness)"
  echo "- ✅ /metrics returns 200 (Prometheus registry bootstrapped)"
  echo "- ✅ RP-11 GET /realm returns 404 tenant_realm_not_found for a nonexistent tenant (router → service → Postgres path verified)"
  echo ""
  echo "> Note: /readyz returns 503 as expected — Keycloak is not started in this smoke environment."
  echo "> Keycloak + OpenBao integration is covered by \`make test-integration\` and the e2e suite."
} >> "$GITHUB_STEP_SUMMARY"
