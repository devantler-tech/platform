#!/usr/bin/env bash
set -euo pipefail

# Pins the exact scope, reused least-privilege grant and mutation shape of the
# mutate-existing C-0211 retrofit for the longhorn-csi-plugin DaemonSet (#3918).

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
readonly script_dir repo_root
readonly policy="${repo_root}/k8s/providers/hetzner/infrastructure/cluster-policies/add-existing-longhorn-csi-plugin-baseline-context.yaml"
readonly infrastructure_kustomization="${repo_root}/k8s/providers/hetzner/infrastructure/kustomization.yaml"
readonly longhorn_dir="${repo_root}/k8s/providers/hetzner/infrastructure/controllers/longhorn"
readonly fixtures_dir="${repo_root}/tests/longhorn-csi-plugin-baseline-context"
readonly grant_name="kyverno:background-controller:mutate-longhorn-csi-plugin"
readonly target='longhorn-csi-plugin'

# Fail closed. On bash 3.2 a `set -e` abort runs the EXIT trap with $? already
# 0, so a trap that only cleans up turns an aborted run into a green one.
completed=0
work_dir=''
cleanup() {
  [[ -n "${work_dir}" ]] && rm -rf "${work_dir}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -f "${policy}" ]] || fail 'the mutate-existing retrofit policy must exist'
grep -Fq 'cluster-policies/add-existing-longhorn-csi-plugin-baseline-context.yaml' "${infrastructure_kustomization}" ||
  fail 'the prod infrastructure layer must deploy the retrofit'

# --- Scope: the policy may never reach beyond the one DaemonSet -------------
[[ "$(yq -r '.spec.mutateExistingOnPolicyUpdate' "${policy}")" == 'true' ]] ||
  fail 'the retrofit must run against the already-created DaemonSet'
[[ "$(yq -r '.spec.failurePolicy' "${policy}")" == 'Ignore' ]] ||
  fail 'the policy must fail open so a Kyverno outage cannot block Longhorn reconciliation'
[[ "$(yq -r '.spec.rules | length' "${policy}")" == '1' ]] ||
  fail 'one serialized rule must apply both baseline fields without racing the target resourceVersion'
[[ "$(yq -r '.spec.rules[0].match.any | length' "${policy}")" == '1' ]] ||
  fail 'the rule must carry exactly one match block'
[[ "$(yq -r '.spec.rules[0].match.any[0].resources.kinds | join(",")' "${policy}")" == 'apps/v1/DaemonSet' ]] ||
  fail 'the rule must match only apps/v1 DaemonSets'
[[ "$(yq -r '.spec.rules[0].match.any[0].resources.namespaces | join(",")' "${policy}")" == 'longhorn-system' ]] ||
  fail 'the rule must match only longhorn-system'
[[ "$(yq -r '.spec.rules[0].match.any[0].resources.names | join(",")' "${policy}")" == "${target}" ]] ||
  fail 'the rule must match only longhorn-csi-plugin'
[[ "$(yq -r '.spec.rules[0].mutate.targets | length' "${policy}")" == '1' ]] ||
  fail 'the rule must declare exactly one target'
[[ "$(yq -r '.spec.rules[0].mutate.targets[0] | .apiVersion + "|" + .kind + "|" + .namespace + "|" + .name' "${policy}")" == "apps/v1|DaemonSet|longhorn-system|${target}" ]] ||
  fail 'the rule must target exactly longhorn-system/longhorn-csi-plugin, statically named'

# --- Shape: only the two non-privilege fields, and never a privilege one ----
[[ "$(yq -r '.spec.rules[0].mutate | keys | sort | join(",")' "${policy}")" == 'foreach,targets' ]] ||
  fail 'the serialized rule must carry only a static target and JSON Patch foreach entries'
if yq -o=json -I=0 '[.spec.rules[].mutate]' "${policy}" |
  grep -Eq 'runAsUser|runAsGroup|"fsGroup"|privileged|allowPrivilegeEscalation|capabilities'; then
  fail 'the retrofit must never supply a privilege, user or group field'
fi
[[ "$(yq -r '.spec.rules[0].mutate.foreach | length' "${policy}")" == '6' ]] ||
  fail 'one rule must cover absent and present pod, container, and initContainer securityContext objects'
[[ "$(yq -r '[.spec.rules[0].mutate.foreach[].patchesJson6902 | select(test("op: (remove|replace)"))] | length' "${policy}")" == '0' ]] ||
  fail 'the retrofit must only add fields, never remove or replace one'

# --- Behaviour: RED/GREEN against the measured live shape ------------------
# The policy has one target, and Kyverno's CLI fake client rejects an object
# that is both trigger and target, so evaluate a copy whose match name alone
# points at a fixture trigger. Everything that mutates is evaluated as shipped.
work_dir="$(mktemp -d)"
trigger_name="$(yq -r '.metadata.name' "${fixtures_dir}/trigger.yaml")"
TRIGGER_NAME="${trigger_name}" yq '.spec.rules[0].match.any[0].resources.names = [strenv(TRIGGER_NAME)]' \
  "${policy}" >"${work_dir}/policy.yaml"
cmp -s <(yq 'del(.spec.rules[0].match)' "${policy}") <(yq 'del(.spec.rules[0].match)' "${work_dir}/policy.yaml") ||
  fail 'the evaluated copy may differ from the shipped policy in its match block only'

apply() {
  local targets="$1" out="$2"
  mkdir -p "${out}"
  if ! kyverno apply "${work_dir}/policy.yaml" \
    --resource "${fixtures_dir}/trigger.yaml" \
    --target-resource "${targets}" \
    --output "${out}" --remove-color >"${out}.log" 2>&1; then
    sed -n '1,80p' "${out}.log" >&2
    fail 'kyverno apply must evaluate the retrofit policy'
  fi
}
docs() {
  yq -r ea "[select(.kind == \"DaemonSet\" and .metadata.namespace == \"$2\" and .metadata.name == \"$3\")] | length" "$1"/*.yaml
}
any() {
  yq -r ea "[select(.kind == \"DaemonSet\" and .metadata.namespace == \"longhorn-system\" and .metadata.name == \"${target}\") | $2] | any" "$1"/*.yaml
}

{ cat "${fixtures_dir}/resources.yaml"; printf -- '---\n'; cat "${fixtures_dir}/near-misses.yaml"; } >"${work_dir}/targets.yaml"
apply "${work_dir}/targets.yaml" "${work_dir}/live"
live="${work_dir}/live"

# `kyverno apply` emits one document per target it actually changed, plus the
# trigger; the positive assertions keep the absence checks from being vacuous.
[[ "$(docs "${live}" longhorn-system "${target}")" == '1' ]] ||
  fail 'the live shape must produce exactly one combined mutation'
[[ "$(any "${live}" '.spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch"')" == 'true' ]] ||
  fail 'the empty pod securityContext must receive fsGroupChangePolicy'
for i in 0 1 2; do
  [[ "$(any "${live}" "(.spec.template.spec.containers[${i}].securityContext // {} | has(\"seLinuxOptions\"))")" == 'true' ]] ||
    fail "container ${i} must receive seLinuxOptions"
  [[ "$(any "${live}" "((.spec.template.spec.containers[${i}].securityContext.seLinuxOptions // {\"x\":1}) | length) == 0")" == 'true' ]] ||
    fail "container ${i}'s seLinuxOptions must stay empty — never pin a level or type"
done
[[ "$(any "${live}" '.spec.template.spec.containers[0].securityContext.privileged == true')" == 'true' ]] ||
  fail 'the registrar must keep its existing securityContext — the object must never be replaced'
[[ "$(any "${live}" '(.spec.template.spec.containers[2].securityContext.privileged == true) and (.spec.template.spec.containers[2].securityContext.allowPrivilegeEscalation == true) and ((.spec.template.spec.containers[2].securityContext.capabilities.add | join(",")) == "SYS_ADMIN")')" == 'true' ]] ||
  fail 'the plugin container must keep every existing securityContext field'
[[ "$(any "${live}" '(.spec.template.spec.containers[1].securityContext | keys | join(",")) == "seLinuxOptions"')" == 'true' ]] ||
  fail 'the liveness probe must gain a securityContext carrying only seLinuxOptions'

# Near misses stay untouched — the engine images are sequenced on #3918.
[[ "$(docs "${live}" tenant-control "${target}")" == '0' ]] ||
  fail 'the same DaemonSet name in another namespace must stay unchanged'
[[ "$(docs "${live}" longhorn-system engine-image-ei-493e04e7)" == '0' ]] ||
  fail 'an engine-image DaemonSet must stay unchanged'

# Convergence: once applied, a second pass patches nothing.
apply "${fixtures_dir}/already-clean.yaml" "${work_dir}/clean"
[[ "$(docs "${work_dir}/clean" longhorn-system "${target}")" == '0' ]] ||
  fail 'an already-retrofitted DaemonSet must not be patched again'

# --- Grant: the reused Role stays exactly as narrow ------------------------
kubectl kustomize "${longhorn_dir}" >"${work_dir}/rendered.yaml" || fail 'the Longhorn controller layer must render'
role_query() { yq -r "select(.kind == \"Role\" and .metadata.name == \"${grant_name}\") | $1" "${work_dir}/rendered.yaml"; }
[[ "$(role_query '.metadata.namespace')" == 'longhorn-system' ]] ||
  fail 'the background mutation grant must be a namespaced Longhorn Role'
[[ "$(role_query '.rules | length')" == '1' ]] ||
  fail 'the mutation Role must carry exactly one rule'
[[ "$(role_query '.rules[0].apiGroups | join(",")')" == 'apps' ]] ||
  fail 'the mutation Role must grant only the apps API group'
[[ "$(role_query '.rules[0].resources | join(",")')" == 'daemonsets' ]] ||
  fail 'the mutation Role must grant only DaemonSets'
[[ "$(role_query '(.rules[0].resourceNames // []) | join(",")')" == "${target}" ]] ||
  fail 'the mutation Role must stay pinned to longhorn-csi-plugin'
[[ "$(role_query '.rules[0].verbs | sort | join(",")')" == 'get,patch,update' ]] ||
  fail 'the mutation Role must grant only get, patch, and update'
binding_query() { yq -r "select(.kind == \"RoleBinding\" and .metadata.name == \"${grant_name}\") | $1" "${work_dir}/rendered.yaml"; }
[[ "$(binding_query '.roleRef.kind + ":" + .roleRef.name')" == "Role:${grant_name}" ]] ||
  fail 'the RoleBinding must reference the namespaced mutation Role'
[[ "$(binding_query '.subjects | length')" == '1' && "$(binding_query '.subjects[0] | .kind + ":" + .namespace + ":" + .name')" == 'ServiceAccount:kyverno:kyverno-background-controller' ]] ||
  fail 'the RoleBinding must bind only the Kyverno background controller'

grep -Fq 'scripts/tests/test-longhorn-csi-plugin-baseline-context.sh' "${repo_root}/.github/workflows/ci.yaml" ||
  fail 'CI must execute the longhorn-csi-plugin retrofit regression test'

completed=1
echo 'longhorn-csi-plugin C-0211 retrofit is exact, additive, convergent, and reuses the pinned grant.'
