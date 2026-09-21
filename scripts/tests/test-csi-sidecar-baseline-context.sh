#!/usr/bin/env bash
set -euo pipefail

# Pins the exact scope, least-privilege grant and mutation shape of the
# mutate-existing C-0211 retrofit for the four Longhorn CSI sidecar
# Deployments (#3946).

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
readonly script_dir repo_root
readonly policy="${repo_root}/k8s/providers/hetzner/infrastructure/cluster-policies/add-existing-csi-sidecar-baseline-context.yaml"
readonly longhorn_dir="${repo_root}/k8s/providers/hetzner/infrastructure/controllers/longhorn"
readonly fixtures_dir="${repo_root}/tests/csi-sidecar-baseline-context"
readonly grant_name="kyverno:background-controller:mutate-csi-sidecars"
readonly expected_targets='csi-attacher,csi-provisioner,csi-resizer,csi-snapshotter'

# Fail closed. On bash 3.2 a `set -e` abort runs the EXIT trap with $? already
# 0, so a trap that only cleans up turns an aborted run into a green one.
completed=0
out_dir=''
rendered=''
cleanup() {
  [[ -n "${out_dir}" ]] && rm -rf "${out_dir}"
  [[ -n "${rendered}" ]] && rm -f "${rendered}"
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

# --- Scope: the policy may never reach beyond the four CSI sidecars ---------
[[ "$(yq -r '.spec.mutateExistingOnPolicyUpdate' "${policy}")" == 'true' ]] ||
  fail 'the retrofit must run against the already-created Deployments'
[[ "$(yq -r '.spec.failurePolicy' "${policy}")" == 'Ignore' ]] ||
  fail 'the policy must fail open so a Kyverno outage cannot block Longhorn reconciliation'
[[ "$(yq -r '.spec.rules | length' "${policy}")" == '1' ]] ||
  fail 'one serialized rule must apply both baseline fields without racing target resource versions'

[[ "$(yq -r '.spec.rules[0].match.any | length' "${policy}")" == '1' ]] ||
  fail 'the rule must carry exactly one match block'
[[ "$(yq -r '.spec.rules[0].match.any[0].resources.kinds | join(",")' "${policy}")" == 'apps/v1/Deployment' ]] ||
  fail 'the rule must match only apps/v1 Deployments'
[[ "$(yq -r '.spec.rules[0].match.any[0].resources.namespaces | join(",")' "${policy}")" == 'longhorn-system' ]] ||
  fail 'the rule must match only longhorn-system'
[[ "$(yq -r '.spec.rules[0].match.any[0].resources.names | sort | join(",")' "${policy}")" == "${expected_targets}" ]] ||
  fail 'the rule must match only the four CSI sidecars'
[[ "$(yq -r '.spec.rules[0].mutate.targets | length' "${policy}")" == '4' ]] ||
  fail 'the rule must declare exactly four targets'
[[ "$(yq -r '.spec.rules[0].mutate.targets[] | .apiVersion + "|" + .kind + "|" + .namespace' "${policy}" | sort -u | paste -sd, -)" == 'apps/v1|Deployment|longhorn-system' ]] ||
  fail 'the rule must target only longhorn-system Deployments'
[[ "$(yq -r '.spec.rules[0].mutate.targets[].name' "${policy}" | sort | paste -sd, -)" == "${expected_targets}" ]] ||
  fail 'the rule must target exactly the four CSI sidecars, statically named'

# --- Shape: only the two non-privilege fields, and never a privilege one ----
[[ "$(yq -r '.spec.rules[0].name' "${policy}")" == 'add-existing-csi-sidecar-baseline-context' ]] ||
  fail 'the serialized rule must describe the complete baseline context'
[[ "$(yq -r '.spec.rules[0].mutate | keys | sort | join(",")' "${policy}")" == 'foreach,targets' ]] ||
  fail 'the serialized rule must carry only static targets and JSON Patch foreach entries'
if yq -o=json -I=0 '[.spec.rules[].mutate]' "${policy}" |
  grep -Eq 'runAsUser|runAsGroup|"fsGroup"|privileged|allowPrivilegeEscalation|capabilities'; then
  fail 'the retrofit must never supply a privilege, user or group field'
fi
[[ "$(yq -r '.spec.rules[0].mutate.foreach | length' "${policy}")" == '6' ]] ||
  fail 'one rule must cover absent and present pod, container, and initContainer securityContext objects'
[[ "$(yq -r '.spec.rules[0].mutate.foreach[0].list' "${policy}")" == '[target]' ]] ||
  fail 'the pod-level patch must execute once per target inside the serialized rule'
[[ "$(yq -r '.spec.rules[0].mutate.foreach[0].patchesJson6902' "${policy}")" == *'/spec/template/spec/securityContext'* ]] ||
  fail 'the serialized rule must create a missing pod securityContext'
[[ "$(yq -r '.spec.rules[0].mutate.foreach[1].patchesJson6902' "${policy}")" == *'/spec/template/spec/securityContext/fsGroupChangePolicy'* ]] ||
  fail 'the serialized rule must add only a missing fsGroupChangePolicy leaf'

# --- Behaviour: RED/GREEN against the measured live shape ------------------
out_dir="$(mktemp -d)"
if ! kyverno apply "${policy}" \
  --resource "${fixtures_dir}/trigger.yaml" \
  --target-resource "${fixtures_dir}/targets.yaml" \
  --output "${out_dir}" --remove-color >"${out_dir}/apply.log" 2>&1; then
  sed -n '1,80p' "${out_dir}/apply.log" >&2
  fail 'kyverno apply must evaluate the retrofit policy'
fi

# `kyverno apply` emits one document per target mutation that actually produced
# a change, plus the trigger. A target the rule left alone emits nothing at all,
# so absence of a document is the proof that nothing was touched — and the
# positive assertions below are the control that keeps that absence meaningful
# rather than vacuous.
docs() {
  yq -r ea "[select(.kind == \"Deployment\" and .metadata.namespace == \"$1\" and .metadata.name == \"$2\")] | length" "${out_dir}"/*.yaml
}
any() {
  local namespace="$1" name="$2" expression="$3"
  yq -r ea "[select(.kind == \"Deployment\" and .metadata.namespace == \"${namespace}\" and .metadata.name == \"${name}\") | ${expression}] | any" "${out_dir}"/*.yaml
}

# A bare sidecar gets both fields.
[[ "$(docs longhorn-system csi-attacher)" == '1' ]] ||
  fail 'a target needing both fields must produce one combined mutation, not competing writes'
[[ "$(any longhorn-system csi-attacher '.spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch"')" == 'true' ]] ||
  fail 'a bare CSI sidecar must receive fsGroupChangePolicy'
[[ "$(any longhorn-system csi-attacher '(.spec.template.spec.containers[0].securityContext // {} | has("seLinuxOptions"))')" == 'true' ]] ||
  fail 'a bare CSI sidecar container must receive seLinuxOptions'
[[ "$(any longhorn-system csi-attacher '((.spec.template.spec.containers[0].securityContext.seLinuxOptions // {"x":1}) | length) == 0')" == 'true' ]] ||
  fail 'seLinuxOptions must stay empty — never pin a level or type'

# The trigger is a target like any other, so it is retrofitted too.
[[ "$(any longhorn-system csi-snapshotter '.spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch"')" == 'true' ]] ||
  fail 'the triggering sidecar must be retrofitted as well'

# An existing container securityContext is added to, never replaced.
[[ "$(any longhorn-system csi-provisioner '((.spec.template.spec.containers[0].securityContext // {}) | has("seLinuxOptions"))')" == 'true' ]] ||
  fail 'a container with other securityContext fields must still receive seLinuxOptions'
[[ "$(any longhorn-system csi-provisioner '.spec.template.spec.containers[0].securityContext.runAsNonRoot == true')" == 'true' ]] ||
  fail 'an existing container securityContext field must survive — the whole object must never be replaced'
[[ "$(any longhorn-system csi-provisioner '((.spec.template.spec.initContainers[0].securityContext // {}) | has("seLinuxOptions"))')" == 'true' ]] ||
  fail 'an initContainer must receive seLinuxOptions too'

# An inherited pod-level seLinuxOptions is never shadowed, and an existing
# fsGroupChangePolicy is never overwritten: both rules must leave csi-resizer
# entirely alone, so it produces no patched document.
[[ "$(docs longhorn-system csi-resizer)" == '0' ]] ||
  fail 'a sidecar that already inherits seLinuxOptions and sets fsGroupChangePolicy must not be patched at all'

# Near misses stay untouched — the storage plane is sequenced on #3918.
[[ "$(docs tenant-control csi-attacher)" == '0' ]] ||
  fail 'the same Deployment name in another namespace must stay unchanged'
[[ "$(docs longhorn-system longhorn-manager)" == '0' ]] ||
  fail 'another longhorn-system Deployment must stay unchanged'

# --- Grant: namespaced, resourceNames-pinned, no cluster-wide writes -------
rendered="$(mktemp)"
kubectl kustomize "${longhorn_dir}" >"${rendered}" || fail 'the Longhorn controller layer must render'

if yq -r '
  select(.kind == "ClusterRole")
  | select([.rules[]?
      | select([.apiGroups[]? | select(test("^(apps|\\*)$"))] | length > 0)
      | select([.resources[]? | select(test("^(deployments|\\*)$"))] | length > 0)
      | select([.verbs[]? | select(test("^(create|delete|deletecollection|patch|update|\\*)$"))] | length > 0)
    ] | length > 0)
  | .metadata.name
' "${rendered}" | grep -q .; then
  fail 'the Longhorn layer must not grant Kyverno cluster-wide Deployment writes'
fi

role_query() { yq -r "select(.kind == \"Role\" and .metadata.name == \"${grant_name}\") | $1" "${rendered}"; }
[[ "$(role_query '.metadata.namespace')" == 'longhorn-system' ]] ||
  fail 'the background mutation grant must be a namespaced Longhorn Role'
[[ "$(role_query '.rules | length')" == '1' ]] ||
  fail 'the mutation Role must carry exactly one rule'
[[ "$(role_query '.rules[0].apiGroups | join(",")')" == 'apps' ]] ||
  fail 'the mutation Role must grant only the apps API group'
[[ "$(role_query '.rules[0].resources | join(",")')" == 'deployments' ]] ||
  fail 'the mutation Role must grant only Deployments'
[[ "$(role_query '(.rules[0].resourceNames // []) | sort | join(",")')" == "${expected_targets}" ]] ||
  fail 'the mutation Role must be pinned to exactly the four CSI sidecars'
[[ "$(role_query '.rules[0].verbs | sort | join(",")')" == 'get,patch,update' ]] ||
  fail 'the mutation Role must grant only get, patch, and update'

binding_query() { yq -r "select(.kind == \"RoleBinding\" and .metadata.name == \"${grant_name}\") | $1" "${rendered}"; }
[[ "$(binding_query '.roleRef.kind + ":" + .roleRef.name')" == "Role:${grant_name}" ]] ||
  fail 'the RoleBinding must reference the namespaced mutation Role'
[[ "$(binding_query '.subjects | length')" == '1' ]] ||
  fail 'the RoleBinding must have exactly one subject'
[[ "$(binding_query '.subjects[0] | .kind + ":" + .namespace + ":" + .name')" == 'ServiceAccount:kyverno:kyverno-background-controller' ]] ||
  fail 'the RoleBinding must bind only the Kyverno background controller'

grep -Fq 'scripts/tests/test-csi-sidecar-baseline-context.sh' "${repo_root}/.github/workflows/ci.yaml" ||
  fail 'CI must execute the CSI sidecar retrofit regression test'

completed=1
echo 'CSI sidecar C-0211 retrofit is exact, least-privilege, additive, and leaves the storage plane untouched.'
