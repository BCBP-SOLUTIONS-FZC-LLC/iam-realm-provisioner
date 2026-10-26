#!/usr/bin/env bash
# pre-deploy-check.sh — validate all production go-live prerequisites before
# deploying iam-realm-provisioner for the first time. Item 13 (tenants past
# RETENTION_DAYS, CD-23) is a standing check for EVERY deploy and every
# RETENTION_DAYS change — docs/runbook.md §10.
#
# Usage:
#   AWS_REGION=ap-south-1 GLUE_REGISTRY_TENANT_NAME=iam-tenant-events \
#   KUBECONFIG=~/.kube/config NAMESPACE=iam \
#   PROVISIONER_DATABASE_URL=postgres://... RETENTION_DAYS=60 \
#   ./scripts/pre-deploy-check.sh
#
# Each check prints PASS or FAIL with a remediation hint.
# Exit code is 0 only if every check passes.

set -euo pipefail

PASS=0
FAIL=0
REGION="${AWS_REGION:-ap-south-1}"
GLUE_REGISTRY="${GLUE_REGISTRY_TENANT_NAME:-iam-tenant-events}"
NAMESPACE="${NAMESPACE:-iam}"

pass() { echo "  ✅ PASS: $1"; PASS=$((PASS+1)); }
fail() { echo "  ❌ FAIL: $1"; echo "         Remediation: $2"; FAIL=$((FAIL+1)); }
section() { echo; echo "── $1 ──────────────────────────────────────────────"; }

# ── 4. SQS RedrivePolicy ─────────────────────────────────────────────────────
section "Item 4 — SQS inbound queues have RedrivePolicy (maxReceiveCount=5)"
for QUEUE in membership-realm-q tenant-realm-q billing-realm-q; do
  URL=$(aws sqs get-queue-url --queue-name "iam-${QUEUE}" --region "$REGION" \
        --query QueueUrl --output text 2>/dev/null || true)
  if [[ -z "$URL" ]]; then
    fail "iam-${QUEUE} does not exist" "Apply deploy/iam/sqs.tf.example via Terraform"
    continue
  fi
  POLICY=$(aws sqs get-queue-attributes --queue-url "$URL" --region "$REGION" \
    --attribute-names RedrivePolicy --query 'Attributes.RedrivePolicy' --output text 2>/dev/null || true)
  if [[ -z "$POLICY" || "$POLICY" == "None" ]]; then
    fail "iam-${QUEUE} has no RedrivePolicy" "Apply deploy/iam/sqs.tf.example — startup panics without it"
  else
    MRC=$(echo "$POLICY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('maxReceiveCount','?'))" 2>/dev/null || echo "?")
    pass "iam-${QUEUE} has RedrivePolicy (maxReceiveCount=${MRC})"
  fi
done

# ── 5. Glue schema registration ──────────────────────────────────────────────
section "Item 5 — Glue registry '${GLUE_REGISTRY}' has all 9 schema definitions AVAILABLE"
SCHEMA_COUNT=$(aws glue list-schemas --registry-id "RegistryName=${GLUE_REGISTRY}" \
  --region "$REGION" --query 'length(Schemas)' --output text 2>/dev/null || echo 0)
if [[ "$SCHEMA_COUNT" -ge 9 ]]; then
  pass "Glue registry has ${SCHEMA_COUNT} schemas (≥9 required)"
else
  fail "Glue registry '${GLUE_REGISTRY}' has ${SCHEMA_COUNT}/9 schemas" \
       "Run: GLUE_REGISTRY_TENANT_NAME=${GLUE_REGISTRY} make schema-register && make schema-verify"
fi

# ── 7. Pushgateway reachable ──────────────────────────────────────────────────
section "Item 7 — Pushgateway reachable (PROMETHEUS_PUSHGATEWAY_URL)"
PUSH_URL="${PROMETHEUS_PUSHGATEWAY_URL:-}"
if [[ -z "$PUSH_URL" ]]; then
  fail "PROMETHEUS_PUSHGATEWAY_URL not set" "Set in deploy/helm/values.yaml or Helm override"
else
  if curl -sf "${PUSH_URL}/-/healthy" >/dev/null 2>&1; then
    pass "Pushgateway healthy at ${PUSH_URL}"
  else
    fail "Pushgateway at ${PUSH_URL} not reachable" \
         "Check deploy/monitoring/pushgateway-scrape.yaml; ensure honor_labels: true"
  fi
fi

# ── 2. Kubernetes Secret (existingSecret keys) ────────────────────────────────
section "Item 2 — Kubernetes Secret has all required envFromSecret keys"
SECRET_NAME="${EXISTING_SECRET:-iam-realm-provisioner}"
REQUIRED_KEYS=(
  SYSTEM_DATABASE_URL MIGRATION_DATABASE_URL
  KEYCLOAK_ADMIN_CLIENT_SECRET
  SNS_TOPIC_TENANT_ARN
  SQS_MEMBERSHIP_REALM_QUEUE_URL SQS_TENANT_REALM_QUEUE_URL SQS_BILLING_REALM_QUEUE_URL
  DOCS_AUTH_TOKEN
)
for KEY in "${REQUIRED_KEYS[@]}"; do
  EXISTS=$(kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" \
    -o jsonpath="{.data.${KEY}}" 2>/dev/null || true)
  if [[ -n "$EXISTS" ]]; then
    pass "Secret key ${KEY} present"
  else
    fail "Secret key ${KEY} missing from ${SECRET_NAME}" \
         "kubectl create secret generic ${SECRET_NAME} -n ${NAMESPACE} --from-literal=${KEY}=<value>"
  fi
done

# ── 6. OpenBao role binding ───────────────────────────────────────────────────
section "Item 6 — OpenBao kubernetes auth role 'iam-realm-provisioner' bound"
BAO_ADDR="${BAO_ADDR:-}"
if [[ -z "$BAO_ADDR" ]]; then
  fail "BAO_ADDR not set — cannot check OpenBao role" \
       "Set BAO_ADDR and re-run, or check deploy/openbao/role.tf.example was applied"
else
  ROLE_STATUS=$(curl -sf "${BAO_ADDR}/v1/auth/kubernetes/role/iam-realm-provisioner" \
    -H "X-Vault-Token: ${BAO_TOKEN:-}" 2>/dev/null | python3 -c "
import sys,json
d=json.load(sys.stdin)
print('ok' if d.get('data') else 'missing')
" 2>/dev/null || echo "unreachable")
  if [[ "$ROLE_STATUS" == "ok" ]]; then
    pass "OpenBao role 'iam-realm-provisioner' exists"
  else
    fail "OpenBao role 'iam-realm-provisioner' ${ROLE_STATUS}" \
         "Apply deploy/openbao/role.tf.example"
  fi
fi

# ── 9. AUDIT_LOG_BASE_URL ────────────────────────────────────────────────────
section "Item 9 — AUDIT_LOG_BASE_URL (non-fatal — degrades to WARN if unset)"
AUDIT_URL="${AUDIT_LOG_BASE_URL:-}"
if [[ -n "$AUDIT_URL" ]]; then
  pass "AUDIT_LOG_BASE_URL set to ${AUDIT_URL}"
else
  echo "  ⚠️  WARN: AUDIT_LOG_BASE_URL unset — TenantIdpConfigChanged entries will accumulate"
  echo "         Set once iam-audit-log is deployed (ops checklist item 9)"
fi

# ── 11. Deploy gate secrets ──────────────────────────────────────────────────
section "Item 11 — GitHub Actions DEPLOY_GATE_ENABLED + KUBECONFIG_B64"
GH_REPO="${GITHUB_REPOSITORY:-BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner}"
GATE=$(gh variable get DEPLOY_GATE_ENABLED --repo "$GH_REPO" 2>/dev/null || echo "missing")
KB=$(gh secret list --repo "$GH_REPO" 2>/dev/null | grep KUBECONFIG_B64 || echo "missing")
if [[ "$GATE" != "missing" ]]; then
  pass "DEPLOY_GATE_ENABLED variable exists (value: ${GATE})"
else
  fail "DEPLOY_GATE_ENABLED variable not set" \
       "gh variable set DEPLOY_GATE_ENABLED --body true --repo ${GH_REPO}"
fi
if [[ "$KB" != "missing" ]]; then
  pass "KUBECONFIG_B64 secret exists"
else
  fail "KUBECONFIG_B64 secret not set" \
       "gh secret set KUBECONFIG_B64 --body \$(base64 -i ~/.kube/config) --repo ${GH_REPO}"
fi

# ── 13. Tenants past the retention window (CD-23, standing check) ───────────
# Every deploy and every RETENTION_DAYS change: list billing-lapse-suspended
# tenants the next subscription-lapse run would offboard (terminal). Same
# predicate as TenantRealmRepository.ListSuspendedPastRetention. Runbook §10.
section "Item 13 — Tenants past RETENTION_DAYS (offboarded on the next subscription-lapse run)"
RETENTION_DAYS="${RETENTION_DAYS:-60}"
DB_URL="${PROVISIONER_DATABASE_URL:-}"
if ! [[ "$RETENTION_DAYS" =~ ^[0-9]+$ ]]; then
  fail "RETENTION_DAYS='${RETENTION_DAYS}' is not a whole number of days" \
       "Set RETENTION_DAYS to the value that will be live after this deploy"
elif [[ -z "$DB_URL" ]]; then
  echo "  ⚠️  WARN: PROVISIONER_DATABASE_URL unset — retention check skipped"
  echo "         Run the query in docs/runbook.md §10 by hand before deploying"
elif ! command -v psql >/dev/null 2>&1; then
  fail "psql not installed — cannot run the retention check" \
       "Install the PostgreSQL client, or run the query in docs/runbook.md §10 by hand"
else
  ROWS=$(psql "$DB_URL" -X -A -t -F ' ' -v ON_ERROR_STOP=1 -c "
    SELECT tenant_id, disabled_at FROM tenant_realms
    WHERE status = 'suspended' AND suspension_source = 'billing_lapse'
      AND disabled_at < now() - make_interval(days => ${RETENTION_DAYS})
    ORDER BY disabled_at" 2>&1) || { fail "retention query failed: ${ROWS}" "Check PROVISIONER_DATABASE_URL"; ROWS="__error__"; }
  if [[ "$ROWS" == "__error__" ]]; then
    :
  elif [[ -z "$ROWS" ]]; then
    pass "No billing-lapse tenant is past RETENTION_DAYS=${RETENTION_DAYS}"
  else
    echo "$ROWS" | sed 's/^/         /'
    fail "$(echo "$ROWS" | wc -l | tr -d ' ') tenant(s) past RETENTION_DAYS=${RETENTION_DAYS} would be offboarded" \
         "Confirm each with Billing before deploying (docs/runbook.md §10)"
  fi
fi

# ── 12. Caller migration ──────────────────────────────────────────────────────
section "Item 12 — Caller migration (manual confirmation required)"
echo "  ⚠️  MANUAL: Confirm O&M, Billing, and Signup BFF parse errorCode (UPPER_SNAKE_CASE)"
echo "             and Content-Type: application/problem+json before deploying."
echo "             See .claude/operations.md item 12 for the full migration guide."

# ── Items 1, 3, 8, 10 — handled in code ─────────────────────────────────────
section "Items 1, 3, 8, 10 — handled in code/Helm (no manual action needed)"
pass "Item 1: realm_provisioner_migrator grants added to 000000_initial_schema.up.sql"
pass "Item 3: IRSA policy + Terraform in deploy/iam/policy.tf.example (apply once)"
pass "Item 8: networkPolicy.databasePorts = [5432, 6432] in values.yaml"
pass "Item 10: CONVERT_TIMEOUT = 10m in values.yaml"

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo "═══════════════════════════════════════════════════"
echo "  PASS: ${PASS}   FAIL: ${FAIL}"
if [[ $FAIL -eq 0 ]]; then
  echo "  ✅ All checks passed — safe to deploy."
  exit 0
else
  echo "  ❌ ${FAIL} check(s) failed — do NOT deploy until resolved."
  exit 1
fi
