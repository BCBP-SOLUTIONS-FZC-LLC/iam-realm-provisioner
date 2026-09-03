# AWS IAM policy for iam-realm-provisioner

This directory holds the reference IAM policy that must be attached to the
service's IRSA role. The application does not create the role itself — that is
managed by platform Terraform. Copy the JSON in `policy.json` into the
Terraform module or apply it directly via `aws iam create-policy` /
`aws iam put-role-policy`.

The role is assumed by the service's Kubernetes ServiceAccount via IRSA. Wire
the ARN into Helm via `serviceAccount.annotations`:

```yaml
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/iam-realm-provisioner
```

The SAME role is assumed by both the `cmd/server` Deployment (publishes to
the `iam-tenant-events` SNS topic; consumes `membership-realm-q`/
`tenant-realm-q`/`billing-realm-q`; writes realm exports to S3 on
offboarding) and all six `cmd/reconciler` CronJobs (§13.1 — one
ServiceAccount, shared by every pod this chart creates).

Note: this role has **no** OpenBao permissions — OpenBao access is a
separate authentication path (Kubernetes auth method against the same
ServiceAccount's projected JWT, not IRSA/STS). See `deploy/openbao/`.

## Grants breakdown

| Action | Purpose | LLD ref |
|---|---|---|
| `sns:Publish` | Outbox → SNS `iam-tenant-events` topic (this service's 9 published events) | §7.3 |
| `sqs:ReceiveMessage` / `DeleteMessage` / `GetQueueAttributes` / `ChangeMessageVisibility` | Three inbound consumers on `membership-realm-q` / `tenant-realm-q` / `billing-realm-q` | §7.1 |
| `sqs:GetQueueAttributes` / `ReceiveMessage` on the three DLQs | Ops visibility into DLQ depth (`deploy/monitoring/app-alerts.yml`'s `{membership,tenant,billing}-realm-q` DLQ alerts) — read-only for triage, not a consume-and-delete grant | §11.5 |
| `glue:GetSchemaVersion` (+ read-only siblings) | `GlueCodec` pre-fetches schema version IDs at startup, refreshed periodically | §13.4 |
| `s3:PutObject` / `GetObject` / `ListBucket` on `REALM_EXPORT_BUCKET` | RP-C3's dedicated-realm export before offboarding delete (RP-INV-5, §8.9/§15.2) | §8.9 |
| `kms:Encrypt` / `GenerateDataKey` on `REALM_EXPORT_KMS_KEY` (scoped to `kms:ViaService: s3.<region>.amazonaws.com`) | Server-side encryption of the realm export object, restricted to S3-driven SSE-KMS calls only | §15.2 |
| `logs:CreateLogStream` / `PutLogEvents` | Container stdout to CloudWatch (if not using an OTel collector for logs) | — |

No S3 `DeleteObject`/`kms:Decrypt` grants — realm exports are write-once,
read-only-for-audit; nothing in this service ever reads its own exports
back, and nothing deletes them (retention/lifecycle is managed by the
bucket's own S3 lifecycle policy, not this application).

## Least-privilege scoping

The SNS topic policy and the SQS queue policies should independently
restrict `Publish`/`ReceiveMessage` to this role and to the actual
publishing services (Org & Membership for `TrialStarted`, the membership
service for the four `DepartmentMembership*`/`TenantRole*` events, Billing
for `TenantReactivated`) respectively — that is managed by platform
infrastructure and is not represented here.
