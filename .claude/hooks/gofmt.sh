#!/usr/bin/env bash
# PostToolUse (Edit|Write|MultiEdit): gofmt the touched Go file so fmt-check
# (pre-commit + CI) never fails on Claude's edits.
set -euo pipefail

file=$(jq -r '.tool_input.file_path // empty')
if [[ "$file" == *.go && -f "$file" ]]; then
  gofmt -w "$file" || true
fi
exit 0
