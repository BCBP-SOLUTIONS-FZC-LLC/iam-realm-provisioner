#!/usr/bin/env bash
# Floci ready-hook — provisions this service's messaging topology (§7.1,
# §7.3), the realm-export S3 bucket, and the Glue Schema Registry + all 9
# event schemas (§7.3.1). Runs automatically on container start via the
# volume mount to /etc/floci/init/ready.d/. Replaces the former
# scripts/init-localstack.sh — Floci is an open-source (MIT), always-free
# AWS emulator; unlike LocalStack Community, Glue Schema Registry ships in
# Floci's free tier, so this single service now covers SNS + SQS + S3 +
# Glue with no Pro tier / auth token needed (docker-compose.pro.yml is
# retired).
#
# Creates:
#   1 SNS topic    — iam-tenant-events (shared, two-producer topic; only
#                    this service's side is provisioned here for local dev)
#   3 inbound SQS  — membership-realm-q, tenant-realm-q, billing-realm-q
#                    (+ DLQs, maxReceiveCount=5, §7.1)
#   SNS->SQS subs  — tenant-realm-q filtered to this service's own topic
#                    (TrialStarted would normally arrive from O&M's
#                    iam.tenant.events topic and membership-realm-q/
#                    billing-realm-q from O&M's/Billing's own topics — in
#                    local dev without those services running, all three
#                    queues are provisioned against this one topic so a
#                    developer can hand-publish test events to any of them).
#   4 fan-out SQS  — LLD §7.4's downstream-consumer mapping for this
#                    service's own 9 produced events: tenant-orgm-q,
#                    tenant-audit-q (unfiltered — every event), tenant-notification-q
#                    (TenantConverted/DirectPaidSignup/TrialExpired/
#                    TenantSuspended/TenantReactivated), tenant-authz-q
#                    (TenantConverted only). In production each of these
#                    belongs to its real consumer (O&M/Audit/Notification/
#                    AuthZ Enrichment); provisioning them here — same
#                    approach iam-org-membership's own init-floci.sh takes
#                    for its own topics — lets local dev observe the full
#                    fan-out without standing up any of those services.
#   1 S3 bucket    — iam-realm-exports (REALM_EXPORT_BUCKET, RP-EVT-4
#                    offboarding target). Not provisioned by the old
#                    init-localstack.sh at all — a pre-existing gap closed
#                    here. No KMS key: undocumented whether Floci's free
#                    tier emulates KMS, and REALM_EXPORT_KMS_KEY already
#                    shipped unset in local dev before this migration —
#                    that stays unchanged, not a regression.
#   1 Glue registry — iam-tenant-events, 9 schemas, one per produced event,
#                    registered from the same JSON Schema Draft-07 files
#                    eventbus.GlueCodec ships, so GLUE_REGISTRY_TENANT_NAME
#                    can stay set in .env and the app runs with the real
#                    Glue wire-format codec locally instead of NoopCodec.
#                    Registered LAST, deliberately (see the comment right
#                    above that section below) — the docker-compose `floci`
#                    healthcheck polls for the last schema registered here
#                    to decide the container is "healthy," and a sibling
#                    repo's own migration shipped a real race-condition bug
#                    by registering Glue first instead.
#
# Matches api/asyncapi.yaml. The docker-compose `floci` service mounts this
# repo's internal/eventschema/ directory read-only at
# /etc/floci/init/schemas so this script can read the schema definitions
# straight from source — one place to update when a schema changes.

set -euo pipefail

AWS_ACCOUNT=000000000000
AWS_REGION=ap-south-1
MAX_RECEIVES=5
SCHEMAS_DIR=/etc/floci/init/schemas
BUCKET=iam-realm-exports

# The AWS CLI baked into the floci compat image defaults AWS_DEFAULT_REGION
# to us-east-1 regardless of FLOCI_DEFAULT_REGION — pin both region env vars
# so every `aws` call below lands in ap-south-1.
export AWS_DEFAULT_REGION="$AWS_REGION"
export AWS_REGION="$AWS_REGION"

echo "[init-floci] provisioning iam-realm-provisioner messaging topology..."

# -- Helpers --------------------------------------------------------------

create_queue_with_dlq() {
  local base="$1"
  local dlq_url dlq_arn queue_url
  dlq_url=$(aws sqs create-queue --queue-name "${base}-dlq" --query QueueUrl --output text)
  dlq_arn=$(aws sqs get-queue-attributes --queue-url "$dlq_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)

  local attrs_file
  attrs_file=$(mktemp)
  cat > "$attrs_file" <<EOF
{"RedrivePolicy": "{\"deadLetterTargetArn\":\"$dlq_arn\",\"maxReceiveCount\":\"5\"}"}
EOF
  queue_url=$(aws sqs create-queue --queue-name "$base" --attributes "file://$attrs_file" --query QueueUrl --output text)
  rm -f "$attrs_file"
  echo "$queue_url"
}

subscribe_queue() {
  local topic_arn="$1" queue_url="$2" filter_json="${3:-}"
  local queue_arn sub_arn
  queue_arn=$(aws sqs get-queue-attributes --queue-url "$queue_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)
  sub_arn=$(aws sns subscribe --topic-arn "$topic_arn" --protocol sqs --notification-endpoint "$queue_arn" --query SubscriptionArn --output text)
  # RawMessageDelivery=true is required: without it SQS wraps the message
  # in an SNS notification envelope (Type/Message/TopicArn/...) instead of
  # delivering the published body verbatim, which breaks platform-events'
  # SQS consumer (it expects the raw EventEnvelope JSON).
  aws sns set-subscription-attributes --subscription-arn "$sub_arn" --attribute-name RawMessageDelivery --attribute-value true >/dev/null
  if [ -n "$filter_json" ]; then
    aws sns set-subscription-attributes --subscription-arn "$sub_arn" --attribute-name FilterPolicy --attribute-value "$filter_json" >/dev/null
  fi
}

# register_schema <schema-file> <schema-name>
register_schema() {
  local file="$1" name="$2"
  aws glue create-schema \
    --registry-id "RegistryName=iam-tenant-events" \
    --schema-name "$name" \
    --data-format JSON \
    --compatibility BACKWARD \
    --schema-definition "file://${SCHEMAS_DIR}/${file}" >/dev/null
  echo "Glue schema $name created (from $(basename "$file"))"
}

# -- SNS topic --------------------------------------------------------------
TENANT_TOPIC_ARN=$(aws sns create-topic --name iam-tenant-events --query TopicArn --output text)

# -- Inbound SQS --------------------------------------------------------------
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
TENANT_NOTIFICATION_Q=$(create_queue_with_dlq tenant-notification-q)
subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_NOTIFICATION_Q" \
  '{"EventType": ["TenantConverted","DirectPaidSignup","TrialExpired","TenantSuspended","TenantReactivated"]}'

# AuthZ Enrichment — TenantConverted only (realm→tenant cache invalidation).
TENANT_AUTHZ_Q=$(create_queue_with_dlq tenant-authz-q)
subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_AUTHZ_Q" '{"EventType": ["TenantConverted"]}'

# User Profile — TenantOffboarded only (GDPR scrub via ScrubTenant, §8.11).
# In production this belongs to iam-user-profile; provisioned here so local
# dev can test the full TenantOffboarded→ScrubTenant path without standing up
# iam-user-profile (RP-PREV-5 fix).
TENANT_LIFECYCLE_UP_Q=$(create_queue_with_dlq tenant-lifecycle-user-profile-q)
subscribe_queue "$TENANT_TOPIC_ARN" "$TENANT_LIFECYCLE_UP_Q" '{"EventType": ["TenantOffboarded"]}'

# -- S3 realm-export bucket (RP-EVT-4) ---------------------------------------
aws s3api create-bucket --bucket "$BUCKET" \
  --create-bucket-configuration LocationConstraint="$AWS_REGION" >/dev/null
echo "S3 bucket: $BUCKET"

# -- Glue Schema Registry — registered LAST, deliberately --------------------
# The floci healthcheck (docker-compose.yml) polls for the last schema
# registered here (TrialTenantProvisioned) to decide the container is
# "healthy" and unblock `app`'s own `depends_on: condition:
# service_healthy`. Registering Glue after every SNS/SQS/S3 resource above
# makes that healthcheck a true signal that the FULL provisioning run has
# actually completed.
aws glue create-registry --registry-name iam-tenant-events >/dev/null
echo "Glue registry iam-tenant-events created"

register_schema direct_paid_signup.json       DirectPaidSignup
register_schema tenant_converted.json         TenantConverted
register_schema tenant_offboarded.json        TenantOffboarded
register_schema tenant_reactivated.json       TenantReactivated
register_schema tenant_realm_ready.json       TenantRealmReady
register_schema tenant_suspended.json         TenantSuspended
register_schema trial_expired.json            TrialExpired
register_schema trial_reactivated.json        TrialReactivated
register_schema trial_tenant_provisioned.json TrialTenantProvisioned

echo ""
echo "[init-floci] done."
echo "  SNS_TOPIC_TENANT_ARN=$TENANT_TOPIC_ARN"
echo "  SQS_MEMBERSHIP_REALM_QUEUE_URL=$MEMBERSHIP_REALM_Q"
echo "  SQS_TENANT_REALM_QUEUE_URL=$TENANT_REALM_Q"
echo "  SQS_BILLING_REALM_QUEUE_URL=$BILLING_REALM_Q"
echo "  SQS_TENANT_ORGM_QUEUE_URL=$TENANT_ORGM_Q (dev-only fan-out: all 9 produced events)"
echo "  SQS_TENANT_AUDIT_QUEUE_URL=$TENANT_AUDIT_Q (dev-only fan-out: all 9 produced events)"
echo "  SQS_TENANT_NOTIFICATION_QUEUE_URL=$TENANT_NOTIFICATION_Q (dev-only fan-out: TenantConverted/DirectPaidSignup/TrialExpired/TenantSuspended/TenantReactivated)"
echo "  SQS_TENANT_AUTHZ_QUEUE_URL=$TENANT_AUTHZ_Q (dev-only fan-out: TenantConverted only)"
echo "  SQS_TENANT_LIFECYCLE_UP_QUEUE_URL=$TENANT_LIFECYCLE_UP_Q (dev-only fan-out: TenantOffboarded — iam-user-profile ScrubTenant)"
echo "  REALM_EXPORT_BUCKET=$BUCKET"
echo "  GLUE_REGISTRY_TENANT_NAME=iam-tenant-events (9 schemas)"
