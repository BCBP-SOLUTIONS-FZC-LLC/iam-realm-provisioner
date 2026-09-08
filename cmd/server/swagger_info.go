// Package main is the entry point for the IAM Realm Provisioner service.
//
// @title           IAM Realm Provisioner API
// @version         1.0
// @description     Realm Provisioner microservice — sole writer to the Keycloak Admin API and system of record for tenant identity lifecycle (trial → active → suspended → offboarded).
// @description
// @description     **Tenant isolation:** All resource access is strictly scoped by x-tenant-id. Cross-tenant access is never permitted.
// @description     **Idempotency:** Every mutating endpoint requires an `Idempotency-Key` header. Resubmitting a completed key replays the stored result without re-executing.
//
// @contact.name   BCBP Solutions
// @contact.email  sharmila.dayalan@bcbpsolutions.com
//
// @license.name   Proprietary
//
// @host      localhost:8080
// @BasePath  /api/v1/internal
//
// @securityDefinitions.apikey SystemRole
// @in                         header
// @name                       x-tenant-roles
// @description                Must contain "iam-system". Injected by the API gateway / mesh; never set by callers directly.
//
// @securityDefinitions.apikey TenantID
// @in                         header
// @name                       x-tenant-id
// @description                Tenant UUID injected by the API gateway.
//
// @securityDefinitions.apikey IdempotencyKey
// @in                         header
// @name                       Idempotency-Key
// @description                Caller-supplied UUID v4. Required on all mutating endpoints. Reusing a key for a completed operation replays the stored result.
//
// @tag.name         Provisioning
// @tag.description  Realm and user provisioning operations (RP-1..RP-4)
//
// @tag.name         IdP
// @tag.description  Identity provider configuration and portability audit (RP-6, RP-7, RP-13)
//
// @tag.name         RealmConfig
// @tag.description  Realm configuration management with local-intent-first reconciliation (RP-8)
//
// @tag.name         MFA
// @tag.description  MFA credential reset (RP-9)
//
// @tag.name         Lifecycle
// @tag.description  Tenant lifecycle operations — reactivate, expire trial, suspend, offboard (RP-10..RP-12, RP-14)
//
// @tag.name         Users
// @tag.description  User management — delete user, revoke sessions (RP-15, RP-16)
//
// @tag.name         Infra
// @tag.description  Health and readiness probes
package main
