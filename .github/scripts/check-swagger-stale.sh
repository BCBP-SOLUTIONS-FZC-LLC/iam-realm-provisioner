#!/usr/bin/env bash
# make swag-check's backing script: fails if regenerating Swagger docs
# from the current handler annotations would change docs/swagger/ - the
# Swagger-side mirror of the inline go.mod-tidy drift check
# (validate-quality.yml) and make schema-verify's AsyncAPI/Glue drift gate.
#
# docs/swagger/{docs.go,swagger.json,swagger.yaml} are generated,
# checked-in output (`make swag`), blank-imported by cmd/server/main.go to
# self-register the spec at init - this catches a handler's @Summary/
# @Success/@Router annotation (or cmd/server/swagger_info.go's global
# metadata) edited without a follow-up `make swag && git add docs/swagger`.
#
# Must invoke swag with the exact same flags/version as the `swag` Makefile
# target - a mismatch here would report false drift.
set -euo pipefail

SWAG_VERSION="v1.16.6"

# swag names the generated package after --output's basename (docs.go's
# "package <dir>"), so the comparison directory MUST also be named
# "swagger" - a generic mktemp -d path would make every run report false
# drift on docs.go's package line alone.
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT
tmp_dir="$tmp_root/swagger"

go run "github.com/swaggo/swag/cmd/swag@${SWAG_VERSION}" init \
  -g swagger_info.go \
  -d cmd/server,internal/adapter/inbound/http \
  --output "$tmp_dir" \
  --parseDependency \
  --parseInternal >/dev/null

if ! diff -rq docs/swagger "$tmp_dir" >/dev/null 2>&1; then
  echo "::error file=docs/swagger,title=Swagger docs stale::docs/swagger/ is stale — run 'make swag' and commit the result."
  diff -rq docs/swagger "$tmp_dir" || true
  exit 1
fi

echo "Swagger docs check passed — docs/swagger/ matches the current handler annotations."
