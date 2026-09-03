#!/usr/bin/env bash
# Fails the release if CHANGELOG.md has no section for RELEASE_TAG.
# Modeled on iam-authz-enrichment's script of the same name (VERSIONING.md
# § Maintainer release process, step 2 — move [Unreleased] into a new
# ## [X.Y.Z] - YYYY-MM-DD section before tagging).
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"

VERSION="${RELEASE_TAG#v}"
if ! grep -q "## \[${VERSION}\]" CHANGELOG.md 2>/dev/null; then
  echo "::error file=CHANGELOG.md::Missing entry for ${VERSION}"
  exit 1
fi
echo "  ✔  changelog entry found for ${VERSION}"
