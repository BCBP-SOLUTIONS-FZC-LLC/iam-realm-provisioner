#!/usr/bin/env bash
# CD-19 Helm check: lint, template rendering, and kubeconform manifest
# validation for all four values combinations:
#   base | base+dev | base+staging | base+prod
#
# Invoked by .github/workflows/validate-helm.yml in CI and by
# `make helm-check` locally. Both paths use the same script so the
# local run is representative of what CI runs.
#
# ── Schema coverage ──────────────────────────────────────────────────────
# kubeconform is run with -ignore-missing-schemas. This means:
#
#   VALIDATED — standard Kubernetes resources (K8s 1.31.0 schemas):
#     Deployment, Service, ServiceAccount, ConfigMap, Secret, CronJob, Job,
#     NetworkPolicy, HorizontalPodAutoscaler, PodDisruptionBudget
#
#   SKIPPED — CRD kinds not present in the default kubeconform schema set:
#     ServiceMonitor      monitoring.coreos.com/v1     (prometheus-operator)
#     PrometheusRule      monitoring.coreos.com/v1     (prometheus-operator)
#     HTTPRoute           gateway.networking.k8s.io/v1 (Gateway API)
#     SecurityPolicy      gateway.envoyproxy.io/v1alpha1 (Envoy Gateway)
#     AuthorizationPolicy security.istio.io/v1beta1   (Istio — CD-19)
#
# None of these five CRD types has an adopted org-wide schema catalog for
# this pipeline. To enable CRD validation, add a -schema-location flag
# pointing to a catalog (e.g. the datreeio/CRDs-catalog) and remove
# -ignore-missing-schemas once a catalog decision is made.
set -euo pipefail

CHART_DIR="deploy/helm"
BASE_VALUES="${CHART_DIR}/values.yaml"
HELM_VERSION="v3.16.3"
KUBECONFORM_VERSION="v0.6.7"
K8S_VERSION="1.31.0"
RELEASE_NAME="iam-realm-provisioner"
NAMESPACE="iam"

# Shared --set overrides applied to BOTH helm lint and helm template.
#
# existingSecret=ci-lint-skip: secret.yaml calls {{- fail ... }} for any
# empty secretValues key when existingSecret is unset. Providing a non-empty
# existingSecret suppresses Secret resource generation entirely (the template's
# own "{{- if not .Values.existingSecret }}" guard) and skips required-value
# validation. This mirrors the production deployment model where existingSecret
# always points to a pre-provisioned Secret (External Secrets Operator / Sealed
# Secrets) — it is the chart's documented bypass for environments that supply
# credentials externally.
SHARED_SETS=(
  "--set" "image.tag=ci-check"
  "--set" "existingSecret=ci-lint-skip"
)

# ── Install tools if absent ───────────────────────────────────────────────
# Follows the same pattern as .github/scripts/arch-lint.sh: check for the
# binary first, install via `go install` only when missing. In CI,
# ~/go/bin is cached by the validate-helm.yml action cache step so
# re-downloads are rare. Locally, the first run compiles the tools once.
echo "::group::Tool setup"
if ! command -v helm >/dev/null 2>&1; then
  echo "helm not found — installing ${HELM_VERSION} via go install..."
  go install "helm.sh/helm/v3/cmd/helm@${HELM_VERSION}"
fi
echo "helm: $(helm version --short 2>/dev/null)"

if ! command -v kubeconform >/dev/null 2>&1; then
  echo "kubeconform not found — installing ${KUBECONFORM_VERSION} via go install..."
  go install "github.com/yannh/kubeconform/cmd/kubeconform@${KUBECONFORM_VERSION}"
fi
# -v exits 0 on recent builds; guard so set -e doesn't fire on older builds.
KC_VER=$(kubeconform -v 2>/dev/null || echo "installed (version flag unsupported)")
echo "kubeconform: ${KC_VER}"
echo "::endgroup::"

# ── Step 1: helm lint ─────────────────────────────────────────────────────
# --strict is intentionally NOT passed: helm's only advisory for this chart is
# "icon is recommended" (Chart.yaml has no icon URL), which is a cosmetic
# registry concern, not a structural chart error. Structural errors (bad YAML,
# undefined template helpers, deprecated API versions) are still caught.
echo "::group::helm lint"
helm lint "${CHART_DIR}" "${SHARED_SETS[@]}"
echo "::endgroup::"
echo "  ✔  helm lint passed"

# ── Step 2: helm template + kubeconform for each values combination ───────
# run_combo <label> [extra -f overlay-values flags...]
# Renders the chart with BASE_VALUES plus any overlay, then pipes the YAML
# to kubeconform. Sets FAILED=1 on any error but always prints ::endgroup::
# so the GitHub Actions log folds remain matched even on failure.
FAILED=0

run_combo() {
  local label="$1"; shift
  echo "::group::helm template + kubeconform [${label}]"

  local rendered
  if ! rendered=$(helm template "${RELEASE_NAME}" "${CHART_DIR}" \
      "${SHARED_SETS[@]}" \
      --namespace "${NAMESPACE}" \
      -f "${BASE_VALUES}" \
      "$@" 2>&1); then
    echo "::error file=${CHART_DIR},title=helm template [${label}]::helm template failed for [${label}] — output below"
    printf '%s\n' "${rendered}"
    FAILED=1
    echo "::endgroup::"
    return
  fi

  if ! printf '%s\n' "${rendered}" \
      | kubeconform \
          -strict \
          -summary \
          -ignore-missing-schemas \
          -kubernetes-version "${K8S_VERSION}" \
          -output pretty; then
    echo "::error file=${CHART_DIR},title=kubeconform [${label}]::Manifest validation failed for [${label}] — see output above"
    FAILED=1
  fi

  echo "::endgroup::"
}

run_combo "base"
run_combo "base+dev"     -f "${CHART_DIR}/values-dev.yaml"
run_combo "base+staging" -f "${CHART_DIR}/values-staging.yaml"
run_combo "base+prod"    -f "${CHART_DIR}/values-prod.yaml"

# ── Final result ──────────────────────────────────────────────────────────
if [ "${FAILED}" -ne 0 ]; then
  echo ""
  echo "::error::Helm check failed — see grouped output above for details."
  exit 1
fi

echo ""
echo "  ✔  helm check passed: base | base+dev | base+staging | base+prod"
echo "  ⚠  CRD schema coverage: ServiceMonitor, PrometheusRule, HTTPRoute,"
echo "     SecurityPolicy, AuthorizationPolicy were SKIPPED (no org-wide"
echo "     CRD schema catalog configured; -ignore-missing-schemas active)."
