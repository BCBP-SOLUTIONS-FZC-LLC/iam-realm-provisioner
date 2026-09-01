# OpenBao (Vault-API-compatible) ACL policy for iam-realm-provisioner.
#
# Grants read/write/delete ONLY under this service's own deterministic
# per-tenant IdP secret path (RP-D10, §12): the KV v2 mount is "secret"
# (BAO_KV_MOUNT) and the logical prefix is "iam/realm-provisioner/idp"
# (BAO_IDP_PATH_PREFIX) — see internal/adapter/outbound/openbao/client.go's
# idpPath(tenantID, idpAlias), which builds
# "<prefix>/<tenant_id>/<idp_alias>" and never anything else.
#
# KV v2 exposes two logical sub-paths per secret: "data/<path>" for
# read/write (WriteIdpSecret/ReadIdpSecret) and "metadata/<path>" for a full
# delete of all versions (DeleteIdpSecret uses the metadata endpoint
# specifically so a deleted IdP secret cannot be recovered via KV v2's
# undelete — RP-7 wants it permanently gone).
#
# Do NOT widen this path pattern (e.g. to "iam/realm-provisioner/*") — the
# service has no other OpenBao-backed secret today. If a future path is
# added (e.g. BAO_KEYCLOAK_ADMIN_PATH, referenced in config but not yet
# wired to a live read in this codebase), extend this policy explicitly
# alongside that change, reviewed by the platform team (see .github/CODEOWNERS).

path "secret/data/iam/realm-provisioner/idp/*" {
  capabilities = ["create", "read", "update"]
}

path "secret/metadata/iam/realm-provisioner/idp/*" {
  capabilities = ["read", "delete", "list"]
}

# Token self-renewal (Kubernetes auth login tokens are short-lived and
# proactively refreshed by Client.token(), §12).
path "auth/token/renew-self" {
  capabilities = ["update"]
}
