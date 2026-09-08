#!/usr/bin/env bash
# Copy this service's produced-event schema files into DEST, renamed to
# their PascalCase Glue name.
#
# schema-gov extract 0.4 writes snake_case filenames under
# internal/eventschema/ (§25's frozen PascalCase event names), but
# schema-gov diff/register need the PascalCase Glue name to match the
# name every other reference to these events uses
# (eventschema.ByEventType's map keys, LLD §25, api/asyncapi.yaml) — so
# this hand-lists the filename->name mapping, the same convention as
# iam-org-membership's identically-named script. A `case` statement is
# used instead of a bash 4+ associative array so this also runs under
# macOS's default /bin/bash 3.2 (no Homebrew bash required) via `make
# schema-register`, not just under CI's ubuntu-latest. A new schema needs
# one line added below. Unlike iam-org-membership (two Glue registries,
# membership/tenant lanes), this service has exactly one registry and one
# lane — its own 9 produced events — so there is no lane argument.
#
# Usage: stage-produced-event-schemas.sh DEST
set -euo pipefail

DEST="${1:?destination directory required}"
SRC="${SCHEMA_SRC:-internal/eventschema}"

name_for() {
  case "$1" in
    trial_tenant_provisioned) echo TrialTenantProvisioned ;;
    tenant_realm_ready)       echo TenantRealmReady ;;
    tenant_converted)         echo TenantConverted ;;
    direct_paid_signup)       echo DirectPaidSignup ;;
    trial_expired)            echo TrialExpired ;;
    trial_reactivated)        echo TrialReactivated ;;
    tenant_suspended)         echo TenantSuspended ;;
    tenant_offboarded)        echo TenantOffboarded ;;
    tenant_reactivated)       echo TenantReactivated ;;
    *)                        echo "" ;;
  esac
}

mkdir -p "$DEST"

shopt -s nullglob
copied=0
for file in "$SRC"/*.json; do
  stem=$(basename "$file" .json)
  name=$(name_for "$stem")
  [ -n "$name" ] || continue
  cp "$file" "$DEST/${name}.json"
  copied=$((copied + 1))
done

if [ "$copied" -eq 0 ]; then
  echo "no produced schemas staged into $DEST" >&2
  exit 1
fi
echo "staged $copied produced schema(s) into $DEST"
