#!/usr/bin/env bash
# Regenerate Swagger docs and fail if committed artifacts are stale.
set -euo pipefail

echo "::group::Regenerate Swagger docs"
make swag
echo "::endgroup::"

if ! git diff --quiet -- docs/swagger/; then
  echo "::error file=docs/swagger/swagger.yaml,title=Swagger stale::Swagger generation succeeded, but docs/swagger has uncommitted changes."
  echo ""
  echo "Swagger docs changed in the working tree:"
  git diff --name-only -- docs/swagger/ | sed 's/^/ - /'
  echo ""
  echo "Run:"
  echo "  git add docs/swagger/docs.go docs/swagger/swagger.json docs/swagger/swagger.yaml"
  echo "  git commit -m \"docs: regenerate swagger artifacts\""
  exit 1
fi

echo "  ✔  Swagger artifacts are up to date"
