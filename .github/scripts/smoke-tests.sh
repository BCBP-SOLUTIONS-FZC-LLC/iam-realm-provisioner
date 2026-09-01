#!/usr/bin/env bash
# Image size + startup gate smoke tests for the CI-built Docker image.
# Invoked by ci.yml once per binary (server / reconciler - both ship in the
# SAME image, per the repo-root Dockerfile) with IMAGE_TAG and BINARY set,
# and ENTRYPOINT set only for the reconciler leg (server uses the image's
# default entrypoint) - keeps shell operators out of inline YAML run blocks.
set -euo pipefail

: "${IMAGE_TAG:?IMAGE_TAG is required}"
: "${BINARY:?BINARY is required}"
ENTRYPOINT_ARGS=()
if [ -n "${ENTRYPOINT:-}" ]; then
  ENTRYPOINT_ARGS=(--entrypoint "${ENTRYPOINT}")
fi

echo "::group::Image size check (${BINARY}, linux/amd64)"
# The limit is deliberately generous so it catches regressions (e.g.
# accidentally COPYing third_party build artefacts or embedding test
# assets), not normal arch variance.
MAX_MB=200
size=$(docker image inspect "${IMAGE_TAG}" --format='{{.Size}}')
mb=$((size / 1024 / 1024))
echo "Image size: ${mb} MB (limit: ${MAX_MB} MB)"
[ "${mb}" -le "${MAX_MB}" ] &
P1=$!
echo "::endgroup::"

echo "::group::Startup gate (${BINARY})"
# The binary must exit non-zero on missing required config, proving its
# config-loading validation actually fires (DATABASE_URL/SNS_TOPIC_ARN/
# SQS queue URLs for the server; --job for the reconciler, checked before
# it ever touches DATABASE_URL - see cmd/reconciler/main.go).
# timeout 10s kills the container if it hangs instead of exiting.
exit_code=0
timeout 10s docker run --rm "${ENTRYPOINT_ARGS[@]}" "${IMAGE_TAG}" 2>/dev/null || exit_code=$?
echo "Container exit code: ${exit_code} (expected non-zero)"
[ "${exit_code}" -ne 0 ] &
P2=$!
echo "::endgroup::"

wait $P1 || {
  echo "::error title=Image size (${BINARY})::Image is ${mb} MB, exceeds ${MAX_MB} MB limit - check COPY instructions for accidental inclusions"
  exit 1
}
wait $P2 || {
  echo "::error title=Startup gate (${BINARY})::Binary exited 0 on missing required env vars - config loading must exit non-zero"
  exit 1
}

{
  echo "### Smoke test results - ${BINARY}"
  echo "- Startup gate: binary exits ${exit_code} on missing required env vars"
  echo "- Image size (linux/amd64): **${mb} MB** (limit: ${MAX_MB} MB)"
} >> "$GITHUB_STEP_SUMMARY"
