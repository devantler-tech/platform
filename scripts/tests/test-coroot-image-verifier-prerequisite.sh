#!/usr/bin/env bash

# Regression contract for staging the two agent compatibility-image signer
# routes before either DaemonSet consumes those images in production.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly admission_verifier="${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml"
readonly node_verifier="${root_dir}/talos/cluster/verify-first-party-images.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || {
  printf '::error::yq is required to run this contract test.\n' >&2
  exit 64
}

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
ghcr.io/devantler-tech/platform-kubescape-node-agent|publishkubescapenodeagent|publish-kubescape-node-agent-hotfix
ghcr.io/devantler-tech/platform-coroot-node-agent|publishcorootnodeagent|publish-coroot-node-agent-hotfix
ROUTES

printf 'Agent compatibility-image verifier prerequisite is valid.\n'
