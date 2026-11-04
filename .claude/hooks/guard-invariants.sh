#!/usr/bin/env bash
# PreToolUse guard (Edit|Write|MultiEdit): blocks edits that CI would reject
# later. Exit 2 = block; stderr is fed back to Claude as the reason.
#   1. RP-INV-1 — Nerzal/gocloak imported outside internal/adapter/outbound/keycloak
#      (same scope as .github/scripts/arch-lint.sh: _test.go files excluded).
#   2. .env — local secrets/config; edit .env-example instead.
#   3. A new migration file — the schema is the single consolidated
#      000000_initial_schema.{up,down}.sql.
# Fails closed: any unexpected error (missing jq, bad JSON) blocks the edit.
# Best-effort only — file changes made via Bash are not seen; CI is the gate.
set -euo pipefail
trap 'echo "guard-invariants.sh failed unexpectedly (line $LINENO); edit blocked. Install jq or fix the hook." >&2; exit 2' ERR

command -v jq >/dev/null || { echo "guard-invariants.sh needs jq (brew install jq); edit blocked." >&2; exit 2; }

input=$(cat)
file=$(jq -r '.tool_input.file_path // empty' <<<"$input")
[ -z "$file" ] && exit 0

# Normalise: absolute, symlinks resolved on the existing parent dir (the file
# itself may not exist yet), so /var vs /private/var and relative paths match.
case "$file" in /*) ;; *) file="${CLAUDE_PROJECT_DIR:-$PWD}/$file" ;; esac
dir=$(dirname "$file")
while [ ! -d "$dir" ]; do dir=$(dirname "$dir"); done
file="$(cd "$dir" && pwd -P)${file#"$dir"}"
# Match on path suffixes, as arch-lint does, so worktrees under
# .claude/worktrees/<name>/ are treated like the main checkout.
path="/${file#/}"

content=$(jq -r '[.tool_input.content, .tool_input.new_string, (.tool_input.edits // [] | .[].new_string)] | map(select(. != null)) | join("\n")' <<<"$input")

if [[ "$path" == *.go && "$path" != *_test.go && "$path" != */internal/adapter/outbound/keycloak/* ]] \
  && grep -q 'Nerzal/gocloak' <<<"$content"; then
  echo "Blocked (RP-INV-1): gocloak may only be imported by internal/adapter/outbound/keycloak. Go through the port.KeycloakAdmin interface instead (add a method to the port + adapter if needed)." >&2
  exit 2
fi

if [[ "$(basename "$path")" == ".env" ]]; then
  echo "Blocked: .env holds local config/secrets and is not edited by Claude. Edit .env-example instead, or ask the user to change .env." >&2
  exit 2
fi

if [[ "$path" == */internal/adapter/outbound/postgres/migrations/* && "$(basename "$path")" != 000000_initial_schema.* ]]; then
  echo "Blocked: this service uses a single consolidated migration. Change internal/adapter/outbound/postgres/migrations/000000_initial_schema.{up,down}.sql instead of adding $(basename "$path")." >&2
  exit 2
fi

exit 0
