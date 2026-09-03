#!/usr/bin/env bash
# Extracts the BuildKit-native SLSA provenance attestation from a pushed
# image and writes provenance.slsa.json.
#
# Uses `docker buildx imagetools inspect`, NOT `cosign download attestation`.
# docker/build-push-action's `provenance`/`sbom` inputs create BuildKit-
# native attestations (manifests attached to the image's own index /
# referrers), a different format from what cosign's `download attestation`
# expects — that command targets attestations cosign itself created via
# `cosign attest`. cosign cannot see BuildKit-native attestations at all,
# regardless of --predicate-type filtering or build caching. `imagetools
# inspect` works for both single- and multi-platform pushed images.
#
# Modeled on iam-authz-enrichment's script of the same name.
set -euo pipefail

: "${IMAGE_NAME:?IMAGE_NAME is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"

IMAGE_REF="${IMAGE_NAME}@${IMAGE_DIGEST}"

PROVENANCE=$(docker buildx imagetools inspect "${IMAGE_REF}" --format '{{ json .Provenance.SLSA }}')

if [ -z "${PROVENANCE}" ] || [ "${PROVENANCE}" = "null" ]; then
  echo "::error title=Provenance export::SLSA provenance attestation not found on ${IMAGE_REF}"
  exit 1
fi

echo "${PROVENANCE}" | jq . > provenance.slsa.json
echo "  ✔  exported SLSA provenance to provenance.slsa.json"
