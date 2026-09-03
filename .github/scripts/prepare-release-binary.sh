#!/usr/bin/env bash
# Cross-compiles release binaries for all target platforms and writes
# per-binary checksums. The artifact directory is the working directory
# when this runs.
#
# Unlike iam-authz-enrichment (one binary, ./cmd/server), this service ships
# TWO binaries from one image (see Dockerfile / Makefile's `build` target) —
# /iam-realm-provisioner-server (HTTP API + 3 SQS consumers + outbox runner)
# and /iam-realm-provisioner-reconciler (the RP-C1..RP-C4 + 2 operational
# CronJobs, dispatched via --job=<name>). Both are cross-compiled here, per
# platform, mirroring the Makefile's own build target's -ldflags convention.
#
# Produces for each platform x binary:
#   iam-realm-provisioner-server_{version}_{os}_{arch}[.exe]
#   iam-realm-provisioner-server_{version}_{os}_{arch}[.exe].sha256
#   iam-realm-provisioner-reconciler_{version}_{os}_{arch}[.exe]
#   iam-realm-provisioner-reconciler_{version}_{os}_{arch}[.exe].sha256
#
# The caller (create-github-release.sh) aggregates all the non-.sha256 files
# into a single checksums.txt with
# `sha256sum iam-realm-provisioner-* | grep -v '\.sha256$'`.
#
# This service is deployed exclusively as a container (README § Docker) —
# these binaries are a convenience for local/non-Docker runs, not the
# primary distribution artifact (that's the signed GHCR image, which
# carries both entrypoints already).
#
# Note: cmd/reconciler/main.go has no `buildVersion` package variable (only
# cmd/server/main.go does), so -X main.buildVersion=... is a silent no-op
# for the reconciler binary today — same as the Makefile's own `build`
# target, which applies the identical flag to both for consistency. Not
# changed here; that's a pre-existing repo quirk, not a release-workflow
# concern.
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"

TARGETS=(
  "linux   amd64"
  "linux   arm64"
  "darwin  amd64"
  "darwin  arm64"
  "windows amd64"
)

BINARIES=(
  "server     ./cmd/server"
  "reconciler ./cmd/reconciler"
)

echo "Building release binaries for tag ${RELEASE_TAG}"

: > release-asset.name

for binary in "${BINARIES[@]}"; do
  read -r NAME PKG <<< "$binary"

  for target in "${TARGETS[@]}"; do
    read -r GOOS GOARCH <<< "$target"
    EXT=""
    [ "${GOOS}" = "windows" ] && EXT=".exe"
    ASSET_NAME="iam-realm-provisioner-${NAME}_${RELEASE_TAG}_${GOOS}_${GOARCH}${EXT}"

    echo "  → ${NAME} ${GOOS}/${GOARCH}"
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
      go build -trimpath \
        -ldflags "-s -w -X main.buildVersion=${RELEASE_TAG}" \
        -o "${ASSET_NAME}" \
        "${PKG}"

    chmod +x "${ASSET_NAME}"
    sha256sum "${ASSET_NAME}" > "${ASSET_NAME}.sha256"
    echo "    ✔  ${ASSET_NAME}"
  done

  # Record the primary (linux/amd64) asset name per binary for downstream
  # steps that need a canonical file reference per entrypoint.
  echo "iam-realm-provisioner-${NAME}_${RELEASE_TAG}_linux_amd64" >> release-asset.name
done

echo "All binaries prepared."
