#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
readonly script_dir repo_root
readonly policies_dir="${repo_root}/k8s/providers/hetzner/infrastructure/cluster-policies"
readonly admission="${policies_dir}/remove-longhorn-registrar-prestop.yaml"
readonly retrofit="${policies_dir}/remove-existing-longhorn-registrar-prestop.yaml"
readonly longhorn_dir="${repo_root}/k8s/providers/hetzner/infrastructure/controllers/longhorn"
readonly helm_release="${longhorn_dir}/helm-release.yaml"
readonly fixtures="${repo_root}/tests/longhorn-registrar-prestop/resources.yaml"
readonly grant_name="kyverno:background-controller:mutate-longhorn-csi-plugin"
readonly expected_chart_version="1.12.1"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -f "${admission}" ]] || fail 'the exact admission policy must exist'
[[ -f "${retrofit}" ]] || fail 'the exact mutate-existing retrofit policy must exist'

# This workaround is intentionally coupled to the upstream Longhorn release
# whose generated DaemonSet carries the impossible /bin/sh hook. A chart bump
# must re-evaluate whether upstream fixed it instead of retaining stale mutation.
[[ "$(yq -r '.spec.chart.spec.version' "${helm_release}")" == "${expected_chart_version}" ]] ||
  fail "the workaround must be re-evaluated before changing Longhorn ${expected_chart_version}"
for policy in "${admission}" "${retrofit}"; do
  [[ "$(yq -r '.metadata.annotations."platform.devantler.tech/longhorn-chart-version"' "${policy}")" == "${expected_chart_version}" ]] ||
    fail "$(basename "${policy}") must declare the reviewed Longhorn chart version"
done

resource_scope() {
  yq -o=json -I=0 \
    '[.spec.rules[0].match.any[0].resources | {"kinds": .kinds, "names": .names, "namespaces": .namespaces}]' "$1"
}

readonly expected_scope='[{"kinds":["apps/v1/DaemonSet"],"names":["longhorn-csi-plugin"],"namespaces":["longhorn-system"]}]'
[[ "$(resource_scope "${admission}")" == "${expected_scope}" ]] ||
  fail 'the admission policy must match only longhorn-system/DaemonSet/longhorn-csi-plugin'
[[ "$(resource_scope "${retrofit}")" == "${expected_scope}" ]] ||
  fail 'the retrofit policy must match only longhorn-system/DaemonSet/longhorn-csi-plugin'

[[ "$(yq -r '.spec.mutateExistingOnPolicyUpdate' "${retrofit}")" == 'true' ]] ||
  fail 'the retrofit policy must run against the existing DaemonSet'
[[ "$(yq -r '.spec.rules[0].mutate.targets | length' "${retrofit}")" == '1' ]] ||
  fail 'the retrofit policy must have exactly one target'
[[ "$(yq -r '.spec.rules[0].mutate.targets[0] | .apiVersion + "/" + .kind + ":" + .namespace + ":" + .name' "${retrofit}")" == 'apps/v1/DaemonSet:longhorn-system:longhorn-csi-plugin' ]] ||
  fail 'the retrofit target must be the exact generated Longhorn CSI DaemonSet'

for policy in "${admission}" "${retrofit}"; do
  [[ "$(yq -r '.spec.failurePolicy' "${policy}")" == 'Ignore' ]] ||
    fail "$(basename "${policy}") must fail open so a Kyverno outage cannot block Longhorn reconciliation"
  [[ "$(yq -r '.spec.rules[0].mutate.patchStrategicMerge.spec.template.spec.containers | length' "${policy}")" == '1' ]] ||
    fail "$(basename "${policy}") must patch exactly one container"
  [[ "$(yq -r '.spec.rules[0].mutate.patchStrategicMerge.spec.template.spec.containers[0].name' "${policy}")" == 'node-driver-registrar' ]] ||
    fail "$(basename "${policy}") must patch only node-driver-registrar"
  [[ "$(yq -r '.spec.rules[0].mutate.patchStrategicMerge.spec.template.spec.containers[0].lifecycle.preStop' "${policy}")" == 'null' ]] ||
    fail "$(basename "${policy}") must delete only lifecycle.preStop"
done

out_dir="$(mktemp -d)"
trap 'rm -rf "${out_dir}"' EXIT
if ! kyverno apply "${admission}" --resource "${fixtures}" --output "${out_dir}" --remove-color >"${out_dir}/apply.log" 2>&1; then
  sed -n '1,80p' "${out_dir}/apply.log" >&2
  fail 'kyverno apply must evaluate the admission policy'
fi

query_daemonset() {
  local namespace="$1" name="$2" expression="$3"
  local value
  value="$(yq -r ea "select(.kind == \"DaemonSet\" and .metadata.namespace == \"${namespace}\" and .metadata.name == \"${name}\") | ${expression}" "${out_dir}"/*.yaml)"
  [[ -n "${value}" ]] || fail "rendered DaemonSet ${namespace}/${name} must be present"
  printf '%s\n' "${value}"
}

[[ "$(query_daemonset longhorn-system longhorn-csi-plugin '(.spec.template.spec.containers[] | select(.name == "node-driver-registrar") | .lifecycle.preStop)')" == 'null' ]] ||
  fail 'the generated Longhorn registrar pre-stop hook must be absent after mutation'
[[ "$(query_daemonset longhorn-system longhorn-csi-plugin '(.spec.template.spec.containers[] | select(.name == "longhorn-csi-plugin") | .lifecycle.preStop.exec.command | join(" "))')" == '/bin/sh -c rm -f /csi//*' ]] ||
  fail 'the valid Longhorn manager pre-stop hook must remain unchanged'
[[ "$(query_daemonset tenant-control longhorn-csi-plugin '(.spec.template.spec.containers[] | select(.name == "node-driver-registrar") | .lifecycle.preStop.exec.command | join(" "))')" == '/bin/sh -c keep-wrong-namespace' ]] ||
  fail 'the same DaemonSet name in another namespace must remain unchanged'
[[ "$(query_daemonset longhorn-system unrelated-csi-plugin '(.spec.template.spec.containers[] | select(.name == "node-driver-registrar") | .lifecycle.preStop.exec.command | join(" "))')" == '/bin/sh -c keep-wrong-name' ]] ||
  fail 'another DaemonSet in longhorn-system must remain unchanged'

rendered="$(mktemp)"
trap 'rm -rf "${out_dir}"; rm -f "${rendered}"' EXIT
kubectl kustomize "${longhorn_dir}" >"${rendered}" || fail 'the Longhorn controller layer must render'

if yq -r '
  select(.kind == "ClusterRole")
  | select([.rules[]?
      | select([.apiGroups[]? | select(test("^(apps|\\*)$"))] | length > 0)
      | select([.resources[]? | select(test("^(daemonsets|\\*)$"))] | length > 0)
      | select([.verbs[]? | select(test("^(create|delete|deletecollection|patch|update|\\*)$"))] | length > 0)
    ] | length > 0)
  | .metadata.name
' "${rendered}" | grep -q .; then
  fail 'the Longhorn layer must not grant Kyverno cluster-wide DaemonSet writes'
fi

[[ "$(yq -r "select(.kind == \"Role\" and .metadata.name == \"${grant_name}\") | .metadata.namespace" "${rendered}")" == 'longhorn-system' ]] ||
  fail 'the background mutation grant must be a namespaced Longhorn Role'
[[ "$(yq -r "select(.kind == \"Role\" and .metadata.name == \"${grant_name}\") | .rules[0].apiGroups | join(\",\")" "${rendered}")" == 'apps' ]] ||
  fail 'the mutation Role must grant only the apps API group'
[[ "$(yq -r "select(.kind == \"Role\" and .metadata.name == \"${grant_name}\") | .rules[0].resources | join(\",\")" "${rendered}")" == 'daemonsets' ]] ||
  fail 'the mutation Role must grant only DaemonSets'
[[ "$(yq -r "select(.kind == \"Role\" and .metadata.name == \"${grant_name}\") | .rules[0].resourceNames | join(\",\")" "${rendered}")" == 'longhorn-csi-plugin' ]] ||
  fail 'the mutation Role must grant only the generated Longhorn CSI DaemonSet'
[[ "$(yq -r "select(.kind == \"Role\" and .metadata.name == \"${grant_name}\") | .rules[0].verbs | sort | join(\",\")" "${rendered}")" == 'get,patch,update' ]] ||
  fail 'the mutation Role must grant only get, patch, and update'
[[ "$(yq -r "select(.kind == \"RoleBinding\" and .metadata.name == \"${grant_name}\") | .roleRef.kind + \":\" + .roleRef.name" "${rendered}")" == "Role:${grant_name}" ]] ||
  fail 'the RoleBinding must reference the namespaced mutation Role'
[[ "$(yq -r "select(.kind == \"RoleBinding\" and .metadata.name == \"${grant_name}\") | .subjects | length" "${rendered}")" == '1' ]] ||
  fail 'the RoleBinding must have exactly one subject'
[[ "$(yq -r "select(.kind == \"RoleBinding\" and .metadata.name == \"${grant_name}\") | .subjects[0] | .kind + \":\" + .namespace + \":\" + .name" "${rendered}")" == 'ServiceAccount:kyverno:kyverno-background-controller' ]] ||
  fail 'the RoleBinding must bind only the Kyverno background controller'

grep -Fq 'scripts/tests/test-longhorn-registrar-prestop-policy.sh' "${repo_root}/.github/workflows/ci.yaml" ||
  fail 'CI must execute the Longhorn registrar regression test'

echo 'Longhorn registrar hook removal is exact, version-coupled, least-privilege, and preserves near misses.'
