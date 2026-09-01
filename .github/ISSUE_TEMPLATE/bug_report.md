---
name: Bug report
about: Report a defect in the iam-realm-provisioner service (HTTP API, Keycloak/OpenBao adapters, SQS consumers, reconcilers, or database layer)
title: '[BUG] '
labels: bug
assignees: ''
---

## Description
A clear description of the bug.

## Service version
`github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner` — tag / commit SHA:

## Go version
`go version goX.Y.Z ...`

## Environment
- [ ] Local dev (`make run`)
- [ ] Docker Compose (`make docker-up`)
- [ ] Staging
- [ ] Production

## Affected area
- [ ] HTTP API (endpoint: `METHOD /api/v1/tenants/{tenant_id}/realm...` — which RP-N?)
- [ ] Keycloak admin adapter (`internal/adapter/outbound/keycloak/`)
- [ ] OpenBao adapter (`internal/adapter/outbound/openbao/`)
- [ ] Event publishing (event type: `Trial*`/`Tenant*`/`DirectPaidSignup`)
- [ ] SQS consumer (membership-realm-q / tenant-realm-q / billing-realm-q)
- [ ] Reconciler (RP-C1..RP-C4 — which job?)
- [ ] Database / migrations / RLS
- [ ] Outbox relay

## Steps to reproduce
1.
2.
3.

## Expected behaviour
What you expected to happen.

## Actual behaviour
What actually happened. Include error messages, HTTP status codes (and §17 error code, e.g. `realm_provisioning_failed`), log output, or stack traces.

```
// paste relevant log output or error here
```

## Minimal reproduction
```go
// paste the smallest snippet or curl command that triggers the bug
```

## Additional context
Any other relevant context (Postgres version, PgBouncer mode, Keycloak version, OpenBao mount/role, related issues).
