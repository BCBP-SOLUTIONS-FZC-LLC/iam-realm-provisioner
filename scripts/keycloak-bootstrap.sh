#!/usr/bin/env bash
# One-time local-dev bootstrap: creates the "iam-realm-provisioner" master-realm
# confidential client (service account) this adapter authenticates as
# (S10.2 - client-credentials grant), and grants its service account the
# master-realm `create-realm` role. Run once after `make docker-up`:
#
#   docker compose exec keycloak bash -c "$(cat scripts/keycloak-bootstrap.sh)"
#
# Idempotent - safe to re-run (create steps no-op if the client/role
# assignment already exists).
set -euo pipefail

KC=/opt/keycloak/bin/kcadm.sh
CLIENT_ID="${KEYCLOAK_ADMIN_CLIENT_ID:-iam-realm-provisioner}"
CLIENT_SECRET="${KEYCLOAK_ADMIN_CLIENT_SECRET:-dev-secret-change-me}"

"$KC" config credentials --server http://localhost:8080 --realm master --user admin --password admin

if ! "$KC" get clients -r master -q clientId="$CLIENT_ID" --fields id | grep -q id; then
  "$KC" create clients -r master \
    -s clientId="$CLIENT_ID" \
    -s enabled=true \
    -s publicClient=false \
    -s serviceAccountsEnabled=true \
    -s secret="$CLIENT_SECRET"
  echo "created client $CLIENT_ID"
fi

CLIENT_UUID=$("$KC" get clients -r master -q clientId="$CLIENT_ID" --fields id --format csv --noquotes | tail -n1)
SA_USER_ID=$("$KC" get "clients/$CLIENT_UUID/service-account-user" -r master --fields id --format csv --noquotes | tail -n1)

# `create-realm` is a realm-level role of the master realm itself - grants
# the service account the ability to create new realms (S10.2, confirmed
# minimal role set: create-realm at master + realm-admin on managed
# realms, the latter auto-conferred since this account is the sole
# creator, RP-INV-1). --uid is authoritative here - the service-account
# username convention (service-account-<clientId>) is an implementation
# detail we don't need to guess at when we already resolved the user id.
"$KC" add-roles -r master --uid "$SA_USER_ID" --rolename create-realm

echo "bootstrap complete: client=$CLIENT_ID secret=$CLIENT_SECRET"
