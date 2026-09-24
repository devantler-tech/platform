#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly node_agent_config="${root_dir}/k8s/bases/infrastructure/controllers/velero/config-map.yaml"
readonly helm_release="${root_dir}/k8s/bases/infrastructure/controllers/velero/helm-release.yaml"
readonly volume_policy="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/velero/config-map.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the Velero node-agent configuration'
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production storage contract'

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

hcloud_skip_rules="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .conditions.storageClass[0] == "hcloud" and
      (.conditions.storageClass | length) == 1 and
      .action.type == "skip"
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly hcloud_skip_rules

[[ "${hcloud_skip_rules}" == '1' ]] ||
  fail "the production Velero policy must contain exactly one hcloud skip rule; found ${hcloud_skip_rules}"

rendered_infrastructure="$(mktemp)"
readonly rendered_infrastructure
trap 'rm -f "${rendered_infrastructure}"' EXIT
kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure" >"${rendered_infrastructure}" ||
  fail 'the production infrastructure overlay did not render'

hcloud_pvcs="$(
  yq -r '
    select(.kind == "PersistentVolumeClaim" and .spec.storageClassName == "hcloud") |
    .metadata.namespace + "/" + .metadata.name
  ' "${rendered_infrastructure}" | sort
)" || fail 'the production hcloud PVC inventory could not be read'
readonly hcloud_pvcs

[[ "${hcloud_pvcs}" == 'openbao/vault-snapshots' ]] ||
  fail "the hcloud skip rule is safe only for the independently mirrored OpenBao snapshot PVC; found: ${hcloud_pvcs:-none}"

longhorn_snapshot_rules="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .conditions.storageClass[0] == "longhorn" and
      (.conditions.storageClass | length) == 1 and
      .action.type == "snapshot"
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly longhorn_snapshot_rules

[[ "${longhorn_snapshot_rules}" == '1' ]] ||
  fail "the production Velero policy must retain exactly one Longhorn snapshot rule; found ${longhorn_snapshot_rules}"

empty_dir_skip_rules="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .conditions.volumeTypes[0] == "emptyDir" and
      (.conditions.volumeTypes | length) == 1 and
      .action.type == "skip"
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly empty_dir_skip_rules

[[ "${empty_dir_skip_rules}" == '1' ]] ||
  fail "the production Velero policy must retain exactly one emptyDir skip rule; found ${empty_dir_skip_rules}"

printf 'Velero data-mover resource contract is valid.\n'
