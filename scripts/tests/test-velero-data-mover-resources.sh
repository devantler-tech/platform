#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly node_agent_config="${root_dir}/k8s/bases/infrastructure/controllers/velero/config-map.yaml"
readonly helm_release="${root_dir}/k8s/bases/infrastructure/controllers/velero/helm-release.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the Velero node-agent configuration'

if ! grep -Fq "'scripts/tests/test-velero-data-mover-resources.sh'" "${ci_workflow}" ||
  ! grep -Fq 'bash scripts/tests/test-velero-data-mover-resources.sh' "${ci_workflow}"; then
  fail 'CI must detect and execute the Velero data-mover resource contract'
fi

memory_limit="$(yq -er '.data["node-agent-config.json"] | from_json | .podResources.memoryLimit' "${node_agent_config}")" ||
  fail 'the Velero data-mover memory limit is missing'
readonly memory_limit

[[ "${memory_limit}" == '1Gi' ]] ||
  fail "Velero data-mover pods need a 1Gi memory limit; found ${memory_limit}"

reload_target="$(yq -er '.spec.values.nodeAgent.annotations."configmap.reloader.stakater.com/reload"' "${helm_release}")" ||
  fail 'the Velero node-agent must reload when its external ConfigMap changes'
readonly reload_target

[[ "${reload_target}" == 'node-agent-config' ]] ||
  fail "the Velero node-agent must reload only node-agent-config; found ${reload_target}"

config_payload="$(yq -er '.data["node-agent-config.json"]' "${node_agent_config}")" ||
  fail 'the Velero node-agent configuration payload is missing'
readonly config_payload

if command -v sha256sum >/dev/null 2>&1; then
  config_hash="$(printf '%s' "${config_payload}" | sha256sum | cut -c1-16)"
elif command -v shasum >/dev/null 2>&1; then
  config_hash="$(printf '%s' "${config_payload}" | shasum -a 256 | cut -c1-16)"
else
  fail 'sha256sum or shasum is required to validate the Velero node-agent rollout generation'
fi
readonly config_hash
expected_generation="config-${config_hash}"
readonly expected_generation

actual_generation="$(yq -er '.spec.values.nodeAgent.podLabels."platform.devantler.tech/node-agent-config-generation"' "${helm_release}")" ||
  fail 'the Velero node-agent pod template must carry the current ConfigMap generation'
readonly actual_generation

[[ "${actual_generation}" == "${expected_generation}" ]] ||
  fail "the Velero node-agent rollout generation is stale: expected ${expected_generation}, found ${actual_generation}"

printf 'Velero data-mover resource contract is valid.\n'
