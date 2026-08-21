#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root

for tool in ksail kubectl yq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "${tool} is required to validate the rendered Kyverno admission resources" >&2
    exit 1
  fi
done

vpa_release="${repo_root}/k8s/bases/infrastructure/controllers/vertical-pod-autoscaler/helm-release.yaml"
certgen_run_as_non_root="$(
  yq -N -r '.spec.values.admissionController.certGen.securityContext.runAsNonRoot // "missing"' \
    "${vpa_release}"
)"
if [[ "${certgen_run_as_non_root}" != "true" ]]; then
  echo "VPA admission certgen container runAsNonRoot=${certgen_run_as_non_root}; want true for Kyverno admission" >&2
  exit 1
fi

rendered_vpas="$(
  kubectl kustomize \
    "${repo_root}/k8s/providers/hetzner/infrastructure/vertical-pod-autoscalers"
)"
controlled_values="$(
  yq ea -N -r '
    select(
      .kind == "VerticalPodAutoscaler" and
      .metadata.name == "kyverno-admission-controller"
    ) |
    .spec.resourcePolicy.containerPolicies[] |
    select(.containerName == "*") |
    .controlledValues
  ' - <<<"${rendered_vpas}"
)"
memory_ceiling="$(
  yq ea -N -r '
    select(
      .kind == "VerticalPodAutoscaler" and
      .metadata.name == "kyverno-admission-controller"
    ) |
    .spec.resourcePolicy.containerPolicies[] |
    select(.containerName == "*") |
    .maxAllowed.memory
  ' - <<<"${rendered_vpas}"
)"

if [[ "${controlled_values}" != "RequestsOnly" ]]; then
  echo "kyverno-admission-controller VPA controlledValues=${controlled_values:-missing}; want RequestsOnly" >&2
  exit 1
fi

if [[ "${memory_ceiling}" != "1Gi" ]]; then
  echo "kyverno-admission-controller VPA maxAllowed.memory=${memory_ceiling:-missing}; want 1Gi" >&2
  exit 1
fi

if ! chart_validation="$(
  ksail workload validate \
    "${repo_root}/k8s/bases/infrastructure/controllers/kyverno" \
    --rules "${repo_root}/scripts/tests/kyverno-admission-vpa-rules.yaml" 2>&1
)"; then
  echo "${chart_validation}" >&2
  exit 1
fi

if [[ "${chart_validation}" != *"observe-kyverno-admission-deployment"* ]] ||
  [[ "${chart_validation}" != *"Deployment/kyverno/kyverno-admission-controller"* ]]; then
  echo "KSail did not observe the rendered Kyverno admission Deployment" >&2
  echo "${chart_validation}" >&2
  exit 1
fi

echo "Kyverno admission VPA keeps request-only resizing and its rendered container's 1Gi memory limit"
