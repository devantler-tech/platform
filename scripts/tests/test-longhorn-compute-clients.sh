#!/usr/bin/env bash
set -euo pipefail

# Volume consumers on compute-only workers need the manager, engine and CSI
# client, but must never gain replica storage just to make attachment work.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
release="${repo_root}/k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml"
policy="${repo_root}/k8s/providers/hetzner/infrastructure/cluster-policies/restrict-storage-to-baseline-workers.yaml"

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
check() {
  yq -o=json '.' "$1" | jq -e '
    .spec.values.defaultSettings.systemManagedComponentsNodeSelector == "" and
    .spec.values.longhornManager.nodeSelector == {} and
    (.spec.values.longhornDriver.nodeSelector // {}) == {} and
    .spec.values.defaultSettings.createDefaultDiskLabeledNodes == true and
    .spec.values.defaultSettings.kubernetesClusterAutoscalerEnabled == true
  ' >/dev/null &&
    yq -o=json '.' "$2" | jq -e '
      .spec.rules[0].match.any[0].resources.names == ["autoscale-*"] and
      .spec.rules[0].mutate.patchStrategicMerge.spec.allowScheduling == false and
      .spec.rules[0].mutate.patchStrategicMerge.spec.evictionRequested == true
    ' >/dev/null
}

check "${release}" "${policy}" || fail 'compute clients must be eligible while replica storage stays excluded'

work_dir="$(mktemp -d)"
completed=0
cleanup() {
  rm -rf "${work_dir}"
  [[ "${completed}" == 1 ]] || exit 1
}
trap cleanup EXIT

# Independent regressions: no client selector or storage-policy relaxation
# may silently turn this safety contract green.
for mutation in \
  '.spec.values.defaultSettings.systemManagedComponentsNodeSelector = "node.longhorn.io/create-default-disk:true"' \
  '.spec.values.longhornManager.nodeSelector = {"node.longhorn.io/create-default-disk": "true"}' \
  '.spec.values.longhornDriver.nodeSelector = {"node.longhorn.io/create-default-disk": "true"}' \
  '.spec.values.defaultSettings.createDefaultDiskLabeledNodes = false' \
  '.spec.values.defaultSettings.kubernetesClusterAutoscalerEnabled = false'; do
  yq "${mutation}" "${release}" >"${work_dir}/release.yaml"
  if check "${work_dir}/release.yaml" "${policy}"; then
    fail "accepted unsafe Helm configuration: ${mutation}"
  fi
done
for mutation in \
  '.spec.rules[0].match.any[0].resources.names = ["wrong-*"]' \
  '.spec.rules[0].mutate.patchStrategicMerge.spec.allowScheduling = true' \
  '.spec.rules[0].mutate.patchStrategicMerge.spec.evictionRequested = false'; do
  yq "${mutation}" "${policy}" >"${work_dir}/policy.yaml"
  if check "${release}" "${work_dir}/policy.yaml"; then
    fail "accepted unsafe replica policy: ${mutation}"
  fi
done

grep -Fq 'scripts/tests/test-longhorn-compute-clients.sh' "${repo_root}/.github/workflows/ci.yaml" || fail 'CI must run this regression'
completed=1
printf 'PASS: compute clients are eligible; eight unsafe client/storage mutations are rejected.\n'
