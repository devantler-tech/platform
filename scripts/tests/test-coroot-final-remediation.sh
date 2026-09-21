#!/usr/bin/env bash

# Regression contract for the final actionable Coroot alert remediations.
#
# Expected Flux controller restarts must not emit readiness failures, and
# cert-manager must use its supported namespace filter rather than mutating
# Cilium's synchronized TLS Secrets.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly flux_instance="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml"
readonly cert_manager_release="${root_dir}/k8s/bases/infrastructure/controllers/cert-manager/helm-release.yaml"
readonly admission_verifier="${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml"
readonly node_verifier="${root_dir}/talos/cluster/verify-first-party-images.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

pass() {
  printf 'ok — %s\n' "$1"
}

for tool in yq jq; do
  command -v "${tool}" >/dev/null 2>&1 || {
    printf '::error::%s is required to run this contract test.\n' "${tool}" >&2
    exit 64
  }
done

kustomize_controller_patch="$({
  yq -er '.spec.kustomize.patches[] | select(.target.kind == "Deployment" and .target.name == "kustomize-controller") | .patch' "${flux_instance}"
})" || fail 'the kustomize-controller patch is missing'
readonly kustomize_controller_patch
[[ "${kustomize_controller_patch}" == *'/spec/template/spec/containers/0/readinessProbe/initialDelaySeconds'* &&
  "${kustomize_controller_patch}" == *$'value: 5'* &&
  "${kustomize_controller_patch}" == *'/spec/template/spec/containers/0/lifecycle'* &&
  "${kustomize_controller_patch}" == *$'preStop:\n      sleep:\n        seconds: 10'* ]] ||
  fail 'kustomize-controller must have startup readiness grace and a ten-second graceful pre-stop'
pass 'expected kustomize-controller restarts have startup and shutdown probe grace'

for controller in kustomize-controller helm-controller notification-controller; do
  controller_patch="$({
    CONTROLLER="${controller}" yq -er '
      .spec.kustomize.patches[] |
      select(.target.kind == "Deployment" and .target.name == strenv(CONTROLLER)) |
      .patch
    ' "${flux_instance}"
  })" || fail "the ${controller} patch is missing"

  printf '%s\n' "${controller_patch}" | yq -e '
    ([.[] | select(.op == "add" and .path == "/spec/minReadySeconds" and .value == 30)] | length) == 1 and
    ([.[] | select(.path == "/spec/replicas" and .value == 2)] | length) == 1 and
    ([.[] | select(.op == "add" and .path == "/spec/template/spec/affinity") |
      .value.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[] |
      select(.topologyKey == "kubernetes.io/hostname") ] | length) == 1
  ' - >/dev/null ||
    fail "${controller} must keep two cross-node replicas and require a 30-second readiness floor"
done
pass 'each replicated Flux controller has a stable cross-node HA rollout contract'

yq -e '
  [.spec.kustomize.patches[] |
   select(.target.kind == "Deployment" and .target.name == "source-controller") |
   .patch | from_yaml | .[] |
   select(.path == "/spec/replicas" or .path == "/spec/minReadySeconds")] |
  length == 0
' "${flux_instance}" >/dev/null ||
  fail 'source-controller must remain outside the replica and readiness-floor patches'
pass 'source-controller remains outside the replicated-controller rollout contract'

cainjector_config="$({
  yq -o=json -I=0 '.spec.values.cainjector.config' "${cert_manager_release}"
})" || fail 'the cainjector configuration is missing'
readonly cainjector_config
printf '%s' "${cainjector_config}" | jq -e '
  .apiVersion == "cainjector.config.cert-manager.io/v1alpha1" and
  .kind == "CAInjectorConfiguration" and
  .ignoreNamespaces == ["cilium-secrets"] and
  (keys | sort) == ["apiVersion", "ignoreNamespaces", "kind"]
' >/dev/null ||
  fail 'cainjector must ignore Secret sources from only the Cilium sync namespace'
pass 'cainjector ignores only Cilium copied Secret sources without mutating them'

generic_expression="$({
  yq -er '.spec.validations[] | select(.expression | contains("attestors.publishapp")) | .expression' "${admission_verifier}"
})" || fail 'the generic first-party image verification route is missing'
readonly generic_expression

while IFS='|' read -r repository attestor workflow; do
  [[ -n "${repository}" ]] || continue
  [[ "${generic_expression}" == *"image != '${repository}'"* &&
    "${generic_expression}" == *"!image.startsWith('${repository}:')"* &&
    "${generic_expression}" == *"!image.startsWith('${repository}@')"* ]] ||
    fail "the generic app signer route must exclude ${repository}"

  expected_identity="^https://github\\.com/devantler-tech/platform/\\.github/workflows/${workflow}\\.yaml@refs/heads/main$"
  actual_identity="$({
    ATTESTOR="${attestor}" yq -er '.spec.attestors[] | select(.name == strenv(ATTESTOR)) | .cosign.keyless.identities[0].subjectRegExp' "${admission_verifier}"
  })" || fail "the ${attestor} admission attestor is missing"
  [[ "${actual_identity}" == "${expected_identity}" ]] ||
    fail "the ${repository} admission identity is not pinned to its main-only publisher"

  dedicated_expression="$({
    ATTESTOR="${attestor}" yq -er '.spec.validations[] | select(.expression | contains("attestors." + strenv(ATTESTOR))) | .expression' "${admission_verifier}"
  })" || fail "the ${attestor} admission route is missing"
  [[ "${dedicated_expression}" == *"image == '${repository}'"* &&
    "${dedicated_expression}" == *"image.startsWith('${repository}:')"* &&
    "${dedicated_expression}" == *"image.startsWith('${repository}@')"* ]] ||
    fail "the ${attestor} admission route must match only its exact repository"

  node_identity="$({
    REPOSITORY="${repository}" yq -er '.rules[] | select(.image == strenv(REPOSITORY)) | .keyless.subjectRegex' "${node_verifier}"
  })" || fail "the Talos verifier rule for ${repository} is missing"
  [[ "${node_identity}" == "${expected_identity}" ]] ||
    fail "the Talos and admission identities differ for ${repository}"
done <<'ROUTES'
ghcr.io/devantler-tech/platform-kubescape-storage|publishkubescapestorage|publish-kubescape-storage-hotfix
ghcr.io/devantler-tech/platform-kubescape-node-agent|publishkubescapenodeagent|publish-kubescape-node-agent-hotfix
ghcr.io/devantler-tech/platform-coroot-node-agent|publishcorootnodeagent|publish-coroot-node-agent-hotfix
ROUTES
pass 'each platform compatibility image has one exact main-only signer route at admission and node pull'

printf 'Final Coroot remediation contract is valid.\n'
