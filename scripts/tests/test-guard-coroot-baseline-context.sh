#!/usr/bin/env bash
# Check guard decisions against API fixtures, with kubectl as the external boundary.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
mkdir "${scratch}/bin"

printf 'metadata:\n  name: observability\n  labels:\n    pod-security.devantler.tech/baseline-context-coroot: enabled\n' \
  >"${scratch}/enabled.yaml"
printf 'metadata:\n  name: observability\n  labels:\n    pod-security.devantler.tech/baseline-context: enabled\n' \
  >"${scratch}/disabled.yaml"
printf 'metadata:\n  name: observability\n  labels:\n    pod-security.devantler.tech/baseline-context-coroot: "true"\n' \
  >"${scratch}/unexpected.yaml"

# Six healthy, operator-owned templates without the two fields (the live shape).
jq -n '
  def owner: [{apiVersion: "coroot.com/v1", kind: "Coroot", name: "coroot", controller: true}];
  def pod: {securityContext: {runAsNonRoot: true},
    containers: [{name: "main", securityContext: {capabilities: {drop: ["ALL"]}}}]};
  def rolled($kind; $name): {kind: $kind,
    metadata: {name: $name, namespace: "observability", generation: 3, resourceVersion: "100",
      uid: ("uid-" + $name), labels: {"app.kubernetes.io/managed-by": "coroot-operator"},
      ownerReferences: owner},
    spec: {replicas: 1, template: {spec: pod}},
    status: {observedGeneration: 3, replicas: 1, readyReplicas: 1, updatedReplicas: 1, availableReplicas: 1}};
  {apiVersion: "v1", kind: "List", items: [
    rolled("Deployment"; "coroot-cluster-agent"),
    rolled("Deployment"; "coroot-prometheus"),
    rolled("StatefulSet"; "coroot-clickhouse-keeper"),
    rolled("StatefulSet"; "coroot-clickhouse-shard-0"),
    rolled("StatefulSet"; "coroot-coroot"),
    {kind: "DaemonSet",
     metadata: {name: "coroot-node-agent", namespace: "observability", generation: 2, resourceVersion: "200",
       uid: "uid-coroot-node-agent", labels: {"app.kubernetes.io/managed-by": "coroot-operator"},
       ownerReferences: owner},
     spec: {template: {spec: pod}},
     status: {observedGeneration: 2, desiredNumberScheduled: 3, numberReady: 3, updatedNumberScheduled: 3}}
  ]}' >"${scratch}/unhardened.json"
jq '.items |= map(.spec.template.spec.securityContext.fsGroupChangePolicy = "OnRootMismatch" |
  .spec.template.spec.containers |= map(.securityContext.seLinuxOptions = {}))' \
  "${scratch}/unhardened.json" >"${scratch}/hardened.json"

# Call N reads input.N.json when present, otherwise input.json.
cat >"${scratch}/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == '--context fixture get deployments,statefulsets,daemonsets --namespace observability --selector app.kubernetes.io/managed-by=coroot-operator -o json --request-timeout=20s' ]] || exit 99
count=$(( $(cat "${FIXTURE_DIR}/count" 2>/dev/null || echo 0) + 1 ))
printf '%s' "${count}" >"${FIXTURE_DIR}/count"
case "${SCENARIO}" in
  api-error) exit 1 ;;
  empty-body) exit 0 ;;
esac
if [[ -f "${FIXTURE_DIR}/input.${count}.json" ]]; then cat "${FIXTURE_DIR}/input.${count}.json"; else cat "${FIXTURE_DIR}/input.json"; fi
SH
printf '#!/usr/bin/env bash\nexit 0\n' >"${scratch}/bin/sleep"
chmod +x "${scratch}/bin/kubectl" "${scratch}/bin/sleep"
export PATH="${scratch}/bin:${PATH}" FIXTURE_DIR="${scratch}"
script="${root_dir}/scripts/guard-coroot-baseline-context.sh"

check() {
  local name="$1" phase="$2" expected="$3" scenario="$4" manifest="$5" reason="${6:-}" rc=0
  : >"${scratch}/outputs"
  rm -f "${scratch}/count"
  SCENARIO="${scenario}" GITHUB_OUTPUT="${scratch}/outputs" bash "${script}" "${phase}" \
    --context fixture --namespace-manifest "${scratch}/${manifest}.yaml" >"${scratch}/output" 2>&1 || rc=$?
  if [[ "${expected}" == pass && "${rc}" -ne 0 ]] || [[ "${expected}" == fail && "${rc}" -ne 1 ]]; then
    printf 'FAIL: %s (exit %s)\n' "${name}" "${rc}" >&2
    cat "${scratch}/output" >&2
    exit 1
  fi
  # A failure must be the reason under test, not an unrelated earlier refusal.
  if [[ -n "${reason}" ]] && ! grep -qF "${reason}" "${scratch}/output"; then
    printf 'FAIL: %s failed for the wrong reason\n' "${name}" >&2
    cat "${scratch}/output" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${name}"
}
use() { rm -f "${scratch}"/input.*.json; cp "${scratch}/$1.json" "${scratch}/input.json"; }

use unhardened
check 'label absent disarms without reading the cluster' before-publish pass api-error disabled
grep -qx 'rollout_required=false' "${scratch}/outputs"
[[ ! -f "${scratch}/count" ]]
check 'unexpected label value is refused' before-publish fail normal unexpected 'unexpected baseline-context-coroot value'
check 'healthy reviewed population arms verification' before-publish pass normal enabled
grep -qx 'rollout_required=true' "${scratch}/outputs"
check 'API error is not absence' before-publish fail api-error enabled 'API read failed'
check 'empty API body is not absence' before-publish fail empty-body enabled 'no item list'

jq '.items |= map(select(.metadata.name != "coroot-prometheus"))' "${scratch}/unhardened.json" >"${scratch}/missing.json"
use missing
check 'a missing template is refused' before-publish fail normal enabled 'differ from the reviewed six'
jq '.items += [.items[0] | .metadata.name = "coroot-extra"]' "${scratch}/unhardened.json" >"${scratch}/extra.json"
use extra
check 'a seventh template is refused' before-publish fail normal enabled 'differ from the reviewed six'
jq '.items[1].status.readyReplicas = 0' "${scratch}/unhardened.json" >"${scratch}/unready.json"
use unready
check 'an unready template is refused' before-publish fail normal enabled 'not fully ready'
jq '.items[5].status.numberReady = 2' "${scratch}/unhardened.json" >"${scratch}/ds-unready.json"
use ds-unready
check 'a partially ready DaemonSet is refused' before-publish fail normal enabled 'not fully ready'
jq '.items[2].metadata.ownerReferences = []' "${scratch}/unhardened.json" >"${scratch}/orphan.json"
use orphan
check 'a template the operator does not own is refused' before-publish fail normal enabled 'not operator-owned'
jq '.items[2].metadata.ownerReferences[0].controller = false' "${scratch}/unhardened.json" >"${scratch}/not-controller.json"
use not-controller
check 'a Coroot owner that is not the controller is refused' before-publish fail normal enabled 'not operator-owned'
jq '.items[2].metadata.ownerReferences[0].kind = "HelmRelease"' "${scratch}/unhardened.json" >"${scratch}/other-owner.json"
use other-owner
check 'a controller owner other than Coroot is refused' before-publish fail normal enabled 'not operator-owned'

use unhardened
check 'unhardened templates are not rollout evidence' after-reconcile fail normal enabled 'did not all reach both fields'
jq '.items[4].spec.template.spec.containers += [{name: "sidecar", securityContext: {}}]' \
  "${scratch}/hardened.json" >"${scratch}/partial.json"
use partial
check 'one container without SELinux options fails the predicate' after-reconcile fail normal enabled 'did not all reach both fields'
jq '.items[4].spec.template.spec.securityContext.seLinuxOptions = {} |
  .items[4].spec.template.spec.containers += [{name: "sidecar", securityContext: {}}]' \
  "${scratch}/hardened.json" >"${scratch}/pod-level.json"
use pod-level
check 'pod-level SELinux options satisfy every container' after-reconcile pass normal enabled

use hardened
check 'hardened, ready and stable templates pass' after-reconcile pass normal enabled
grep -qx 'PASS: six Coroot templates carry both fields, are ready, and are stable' "${scratch}/output"

# Rewrites during observation. Call 1 proves readiness; calls 2-4 are samples.
jq '.items[0].metadata.resourceVersion = "101"' "${scratch}/hardened.json" >"${scratch}/v101.json"
jq '.items[0].metadata.resourceVersion = "102"' "${scratch}/hardened.json" >"${scratch}/v102.json"
jq '.items[1].metadata.resourceVersion = "101"' "${scratch}/v101.json" >"${scratch}/two-objects.json"
use hardened; cp "${scratch}/v101.json" "${scratch}/input.2.json"; cp "${scratch}/v101.json" "${scratch}/input.3.json"
cp "${scratch}/v101.json" "${scratch}/input.4.json"
check 'one write on one template is tolerated' after-reconcile pass normal enabled
use hardened; cp "${scratch}/v101.json" "${scratch}/input.2.json"; cp "${scratch}/v102.json" "${scratch}/input.3.json"
check 'repeated writes on one template are a loop' after-reconcile fail normal enabled 'repeated owner writes'
use hardened; for n in 2 3 4; do cp "${scratch}/two-objects.json" "${scratch}/input.${n}.json"; done
check 'one write on each of two templates is not a loop' after-reconcile pass normal enabled
jq '.items[3].metadata.generation = 4 | .items[3].status.observedGeneration = 4' \
  "${scratch}/hardened.json" >"${scratch}/regen.json"
use hardened; cp "${scratch}/regen.json" "${scratch}/input.3.json"
check 'a template rewrite during observation fails' after-reconcile fail normal enabled 'rewrote or replaced'
jq '.items[3].metadata.uid = "uid-replacement"' "${scratch}/hardened.json" >"${scratch}/replaced.json"
use hardened; cp "${scratch}/replaced.json" "${scratch}/input.2.json"
check 'a replaced template during observation fails' after-reconcile fail normal enabled 'rewrote or replaced'
use hardened; cp "${scratch}/unhardened.json" "${scratch}/input.2.json"
check 'an operator stripping the fields fails' after-reconcile fail normal enabled 'removed an admitted field'
use hardened; cp "${scratch}/unready.json" "${scratch}/input.3.json"
jq '.items |= map(.spec.template.spec.securityContext.fsGroupChangePolicy = "OnRootMismatch" |
  .spec.template.spec.containers |= map(.securityContext.seLinuxOptions = {}))' \
  "${scratch}/input.3.json" >"${scratch}/tmp.json" && mv "${scratch}/tmp.json" "${scratch}/input.3.json"
check 'losing readiness during observation fails' after-reconcile fail normal enabled 'lost readiness'
printf 'PASS: Coroot baseline guard fixtures\n'
