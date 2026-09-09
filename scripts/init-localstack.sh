#!/usr/bin/env bash
# LocalStack ready-hook — provisions this service's messaging topology
# (S7, S13). Runs automatically on container start via the volume mount to
# /etc/localstack/init/ready.d/.
#
# Creates:
#   1 SNS topic    — iam-tenant-events (shared, two-producer topic; only
#                    this service's side is provisioned here for local dev)
#   3 inbound SQS  — membership-realm-q, tenant-realm-q, billing-realm-q
#                    (+ DLQs, maxReceiveCount=5, S7.1)
#   SNS->SQS subs  — tenant-realm-q filtered to this service's own topic
#                    (TrialStarted would normally arrive from O&M's
#                    iam.tenant.events topic and membership-realm-q/
#                    billing-realm-q from O&M's/Billing's own topics — in
#                    local dev without those services running, all three
#                    queues are provisioned against this one topic so a
#                    developer can hand-publish test events to any of them).
#   4 fan-out SQS  — LLD §7.4's downstream-consumer mapping for this
#                    service's own 9 produced events: tenant-orgm-q,
#                    tenant-audit-q (unfiltered — every event), tenant-notif-q
#                    (TenantConverted/DirectPaidSignup/TrialExpired/
#                    TenantSuspended/TenantReactivated), tenant-authz-q
#                    (TenantConverted only). In production each of these
#                    belongs to its real consumer (O&M/Audit/Notification/
#                    AuthZ Enrichment); provisioning them here — same
#                    approach iam-org-membership's init-localstack.sh takes
#                    for its own topics — lets local dev observe the full
#                    fan-out without standing up any of those services.
set -euo pipefail

export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
AWS_REGION="${AWS_REGION:-ap-south-1}"
ENDPOINT="--endpoint-url=http://localhost:4566"
awslocal() { aws $ENDPOINT --region "$AWS_REGION" "$@"; }

echo "[init-localstack] provisioning iam-realm-provisioner messaging topology..."

# -- Helpers --------------------------------------------------------------

create_queue_with_dlq() {
  local base="$1"
  local dlq_url dlq_arn queue_url
  dlq_url=$(awslocal sqs create-queue --queue-name "${base}-dlq" --query QueueUrl --output text)
  dlq_arn=$(awslocal sqs get-queue-attributes --queue-url "$dlq_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)

  local attrs_file
  attrs_file=$(mktemp)
  cat > "$attrs_file" <<EOF
{"RedrivePolicy": "{\"deadLetterTargetArn\":\"$dlq_arn\",\"maxReceiveCount\":\"5\"}"}
EOF
  queue_url=$(awslocal sqs create-queue --queue-name "$base" --attributes "file://$attrs_file" --query QueueUrl --output text)
  rm -f "$attrs_file"
  echo "$queue_url"
}

subscribe_queue() {
  local topic_arn="$1" queue_url="$2" filter_json="${3:-}"
  local queue_arn sub_arn
  queue_arn=$(awslocal sqs get-queue-attributes --queue-url "$queue_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)
  sub_arn=$(awslocal sns subscribe --topic-arn "$topic_arn" --protocol sqs --notification-endpoint "$queue_arn" --query SubscriptionArn --output text)
  # RawMessageDelivery=true is required: without it SQS wraps the message
  # in an SNS notification envelope (Type/Message/TopicArn/...) instead of
  # delivering the published body verbatim, which breaks platform-events'
  # SQS consumer (it expects the raw EventEnvelope JSON) — confirmed
  # empirically (a delivered message showed up as
  # {"event_type":"Notification","event_id":""} until this was set).
  awslocal sns set-subscription-attributes --subscription-arn "$sub_arn" --attribute-name RawMessageDelivery --attribute-value true >/dev/null
  if [ -n "$filter_json" ]; then
    awslocal sns set-subscription-attributes --subscription-arn "$sub_arn" --attribute-name FilterPolicy --attribute-value "$filter_json" >/dev/null
  fi
}

# -- SNS topic (S7.3) -----------------------------------------------------
TENANT_TOPIC_ARN=$(awslocal sns create-topic --name iam-tenant-events --query TopicArn --output text)

# -- Inbound SQS (S7.1) ---------------------------------------------------
MEMBERSHIP_REALM_Q=$(create_queue_with_dlq membership-realm-q)
TENANT_REALM_Q=$(create_queue_with_dlq tenant-realm-q)
BILLING_REALM_Q=$(create_queue_with_dlq billing-realm-q)

subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_REALM_Q" '{"EventType": ["TrialStarted"]}'
subscribe_queue "$TENANT_TOPIC_ARN" "$MEMBERSHIP_REALM_Q" '{"EventType": ["DepartmentMembershipGranted","DepartmentMembershipLevelChanged","TenantRoleGranted","TenantRoleRevoked"]}'
subscribe_queue "$TENANT_TOPIC_ARN" "$BILLING_REALM_Q" '{"EventType": ["TenantReactivated"]}'

# -- Outbound fan-out SQS (LLD §7.4) ---------------------------------------
# Downstream-consumer queues for this service's own 9 produced events.
# In real deployments each belongs to its consumer (O&M/Audit/Notification/
# AuthZ Enrichment); provisioned here so local dev/integration tests can
# observe the full fan-out without those services running.

# O&M — consumes all nine (settles/reconciles the tenants row).
TENANT_ORGM_Q=$(create_queue_with_dlq tenant-orgm-q)
subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_ORGM_Q"

# Audit Log — catch-all, every tenant-lifecycle event.
TENANT_AUDIT_Q=$(create_queue_with_dlq tenant-audit-q)
subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_AUDIT_Q"

# Notification — events that generate a user/admin email.
TENANT_NOTIF_Q=$(create_queue_with_dlq tenant-notif-q)
subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_NOTIF_Q" \
  '{"EventType": ["TenantConverted","DirectPaidSignup","TrialExpired","TenantSuspended","TenantReactivated"]}'

# AuthZ Enrichment — TenantConverted only (realm→tenant cache invalidation).
TENANT_AUTHZ_Q=$(create_queue_with_dlq tenant-authz-q)
subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_AUTHZ_Q" '{"EventType": ["TenantConverted"]}'

echo "[init-localstack] done."
echo "  SNS_TOPIC_TENANT_ARN=$TENANT_TOPIC_ARN"
echo "  SQS_MEMBERSHIP_REALM_QUEUE_URL=$MEMBERSHIP_REALM_Q"
echo "  SQS_TENANT_REALM_QUEUE_URL=$TENANT_REALM_Q"
echo "  SQS_BILLING_REALM_QUEUE_URL=$BILLING_REALM_Q"
echo "  SQS_TENANT_ORGM_QUEUE_URL=$TENANT_ORGM_Q (dev-only fan-out: all 9 produced events)"
echo "  SQS_TENANT_AUDIT_QUEUE_URL=$TENANT_AUDIT_Q (dev-only fan-out: all 9 produced events)"
echo "  SQS_TENANT_NOTIF_QUEUE_URL=$TENANT_NOTIF_Q (dev-only fan-out: TenantConverted/DirectPaidSignup/TrialExpired/TenantSuspended/TenantReactivated)"
echo "  SQS_TENANT_AUTHZ_QUEUE_URL=$TENANT_AUTHZ_Q (dev-only fan-out: TenantConverted only)"
