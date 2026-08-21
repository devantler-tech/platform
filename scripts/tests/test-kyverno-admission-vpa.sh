#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root

for tool in helm jq ksail kubectl yq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "${tool} is required to validate the rendered Kyverno admission resources" >&2
    exit 1
  fi
done

vpa_release="${repo_root}/k8s/bases/infrastructure/controllers/vertical-pod-autoscaler/helm-release.yaml"
vpa_repository="${repo_root}/k8s/bases/infrastructure/controllers/vertical-pod-autoscaler/helm-repository.yaml"
vpa_chart="$(yq -N -r '.spec.chart.spec.chart' "${vpa_release}")"
vpa_chart_version="$(yq -N -r '.spec.chart.spec.version' "${vpa_release}")"
vpa_chart_url="$(yq -N -r '.spec.url' "${vpa_repository}")"
if ! rendered_vpa_chart="$(
  yq -o yaml '.spec.values' "${vpa_release}" |
    helm template vertical-pod-autoscaler "${vpa_chart}" \
      --repo "${vpa_chart_url}" \
      --version "${vpa_chart_version}" \
      --namespace vertical-pod-autoscaler \
      --values -
)"; then
  echo "failed to render VPA chart ${vpa_chart_version} for certgen admission validation" >&2
  exit 1
fi

certgen_job_count=0
while IFS=$'\t' read -r job_name container_count container_name run_as_non_root read_only_root no_privilege_escalation dropped_capabilities; do
  [[ -n "${job_name}" ]] || continue
  certgen_job_count=$((certgen_job_count + 1))
  if [[ "${container_count}" -ne 1 || "${run_as_non_root}" != "true" ||
    "${read_only_root}" != "true" || "${no_privilege_escalation}" != "true" ||
    "${dropped_capabilities}" != "ALL" ]]; then
    echo "${job_name}/${container_name} has an incomplete enforced security context" >&2
    exit 1
  fi
done < <(
  yq ea -N -o=json -I=0 '
    select(.kind == "Job" and (.metadata.name | contains("admission-certgen"))) |
    {
      "job_name": .metadata.name,
      "container_count": (.spec.template.spec.containers | length),
      "container_name": .spec.template.spec.containers[0].name,
      "run_as_non_root": (.spec.template.spec.containers[0].securityContext.runAsNonRoot == true),
      "read_only_root": (.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true),
      "no_privilege_escalation": (.spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false),
      "dropped_capabilities": ((.spec.template.spec.containers[0].securityContext.capabilities.drop // []) | join(","))
    }
  ' - <<<"${rendered_vpa_chart}" |
    jq -r '[.job_name, .container_count, .container_name, .run_as_non_root, .read_only_root, .no_privilege_escalation, .dropped_capabilities] | @tsv'
)
if [[ "${certgen_job_count}" -ne 2 ]]; then
  echo "rendered VPA chart has ${certgen_job_count} certgen Jobs; want 2" >&2
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

echo "VPA certgen Jobs satisfy admission policy; Kyverno VPA keeps request-only resizing and its rendered 1Gi limit"
