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
chmod +x "${scratch}/bin/kubectl"
# Advance the guard's own Bash elapsed-time counter instead of waiting in tests.
# An exported function runs in that shell, unlike an external sleep executable.
sleep() { SECONDS=$((SECONDS + $1)); }
export -f sleep
export PATH="${scratch}/bin:${PATH}" FIXTURE_DIR="${scratch}"
script="${root_dir}/scripts/guard-coroot-baseline-context.sh"

extra_args=()
check() {
  local name="$1" phase="$2" expected="$3" scenario="$4" manifest="$5" reason="${6:-}" rc=0
  : >"${scratch}/outputs"
  rm -f "${scratch}/count"
  SCENARIO="${scenario}" GITHUB_OUTPUT="${scratch}/outputs" bash "${script}" "${phase}" \
    --context fixture --namespace-manifest "${scratch}/${manifest}.yaml" \
    ${extra_args[@]+"${extra_args[@]}"} >"${scratch}/output" 2>&1 || rc=$?
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

# A sequential DaemonSet rollout can outlast the old twelve-poll allowance.
jq '.items[5].status.updatedNumberScheduled = 2' "${scratch}/hardened.json" >"${scratch}/rolling.json"
use hardened
for ((n = 1; n <= 20; n++)); do cp "${scratch}/rolling.json" "${scratch}/input.${n}.json"; done
check 'a healthy sequential rollout may take more than one minute' after-reconcile pass normal enabled
[[ "$(cat "${scratch}/count")" == 24 ]] || {
  printf 'FAIL: delayed convergence must still receive three stability samples\n' >&2
  exit 1
}

use rolling
check 'a rollout that never converges still times out' after-reconcile fail normal enabled 'did not all reach both fields'
reads="$(cat "${scratch}/count")"
[[ "${reads}" -gt 12 && "${reads}" -le 120 ]] || {
  printf 'FAIL: convergence must retain a bounded ten-minute polling deadline\n' >&2
  exit 1
}

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

# A template that was already unready before publication. Only all six carrying
# both fields, unrewritten, separates an unrelated fault from an admission loop.
jq '.items[4].spec.replicas = 2 | .items[4].status |= (.replicas = 2 | .readyReplicas = 1 | .updatedReplicas = 2 | .availableReplicas = 1)' \
  "${scratch}/hardened.json" >"${scratch}/degraded.json"
record='[{"key":"StatefulSet/coroot-coroot","generation":3,"uid":"uid-coroot-coroot","ready":1}]'
use degraded
check 'a stable pre-existing unready template does not refuse publication' before-publish pass normal enabled \
  'already unready before this deployment'
grep -qx 'rollout_required=true' "${scratch}/outputs"
grep -qxF "preexisting_unready=${record}" "${scratch}/outputs"
[[ "$(cat "${scratch}/count")" == 4 ]] || {
  printf 'FAIL: a pre-existing unready template must receive three stability samples\n' >&2
  exit 1
}
use degraded; cp "${scratch}/hardened.json" "${scratch}/input.4.json"
check 'a template that recovers during observation leaves no tolerated record' before-publish pass normal enabled
grep -qx 'rollout_required=true' "${scratch}/outputs"
if grep -q '^preexisting_unready=' "${scratch}/outputs"; then
  printf "FAIL: a recovered template must leave no tolerated record\n" >&2
  exit 1
fi
jq '.items[4].metadata.generation = 4 | .items[4].status.observedGeneration = 4' \
  "${scratch}/degraded.json" >"${scratch}/degraded-regen.json"
use degraded; cp "${scratch}/degraded-regen.json" "${scratch}/input.3.json"
check 'an unready template rewritten during observation is refused' before-publish fail normal enabled 'rewrote or replaced'
jq '.items[4].metadata.resourceVersion = "101"' "${scratch}/degraded.json" >"${scratch}/degraded-v101.json"
jq '.items[4].metadata.resourceVersion = "102"' "${scratch}/degraded.json" >"${scratch}/degraded-v102.json"
use degraded; cp "${scratch}/degraded-v101.json" "${scratch}/input.2.json"; cp "${scratch}/degraded-v102.json" "${scratch}/input.3.json"
check 'an unready template written repeatedly is refused' before-publish fail normal enabled 'repeated owner writes'
jq '.items[4].metadata.generation = 4' "${scratch}/degraded.json" >"${scratch}/degraded-unobserved.json"
use degraded-unobserved
check 'an unready template with an unobserved generation is refused' before-publish fail normal enabled 'has not observed the latest generation'
use degraded; cp "${scratch}/unhardened.json" "${scratch}/input.3.json"
check 'an unready template losing its fields during observation is refused' before-publish fail normal enabled 'removed an admitted field'
jq '.items[4].metadata.ownerReferences = []' "${scratch}/degraded.json" >"${scratch}/degraded-orphan.json"
use degraded; cp "${scratch}/degraded-orphan.json" "${scratch}/input.2.json"
check 'an unready template losing its owner during observation is refused' before-publish fail normal enabled 'lost its operator owner'

jq '.items[1].status.readyReplicas = 0' "${scratch}/degraded.json" >"${scratch}/degraded-second.json"
jq '.items[4].status.readyReplicas = 0 | .items[4].status.availableReplicas = 0' "${scratch}/degraded.json" >"${scratch}/degraded-worse.json"
use degraded; cp "${scratch}/degraded-second.json" "${scratch}/input.3.json"
check 'a second template losing readiness during observation is refused' before-publish fail normal enabled 'lost readiness'
use degraded; cp "${scratch}/degraded-worse.json" "${scratch}/input.2.json"
check 'an unready template getting less ready during observation is refused' before-publish fail normal enabled 'lost readiness'

# After reconcile the tolerated record admits only that same object, no worse.
use degraded
check 'an unready template with no tolerated record still fails' after-reconcile fail normal enabled 'did not all reach both fields'
extra_args=(--tolerate-unready "${record}")
check 'a tolerated pre-existing unready template is reported, not passed' after-reconcile pass normal enabled \
  'PREEXISTING-UNREADY: six Coroot templates carry both fields and are stable; StatefulSet/coroot-coroot'
grep -qx 'baseline_result=preexisting-unready' "${scratch}/outputs"
if grep -q '^PASS:' "${scratch}/output"; then
  printf "FAIL: a still-unready template must never be reported as a pass\n" >&2
  exit 1
fi
use hardened
check 'a tolerated template that recovered passes' after-reconcile pass normal enabled
grep -qx 'baseline_result=pass' "${scratch}/outputs"
grep -qx 'PASS: six Coroot templates carry both fields, are ready, and are stable' "${scratch}/output"
use degraded-second
check 'the tolerated record does not cover another template' after-reconcile fail normal enabled 'did not all reach both fields'
use degraded-regen
check 'the tolerated record lapses when the deployment changes the template' after-reconcile fail normal enabled 'did not all reach both fields'
jq '.items[4].metadata.uid = "uid-replacement"' "${scratch}/degraded.json" >"${scratch}/degraded-replaced.json"
use degraded-replaced
check 'the tolerated record lapses when the template is replaced' after-reconcile fail normal enabled 'did not all reach both fields'
use degraded-worse
check 'a tolerated template that got less ready fails' after-reconcile fail normal enabled 'did not all reach both fields'
use degraded; cp "${scratch}/degraded-worse.json" "${scratch}/input.3.json"
check 'a tolerated template losing readiness during observation fails' after-reconcile fail normal enabled 'lost readiness'
use degraded; cp "${scratch}/degraded-v101.json" "${scratch}/input.2.json"; cp "${scratch}/degraded-v102.json" "${scratch}/input.3.json"
check 'a tolerated template is still held to the rewrite loop check' after-reconcile fail normal enabled 'repeated owner writes'
jq '.items |= map(.spec.template.spec.securityContext |= del(.fsGroupChangePolicy))' "${scratch}/degraded.json" >"${scratch}/degraded-stripped.json"
use degraded-stripped
check 'a tolerated template is still held to both fields' after-reconcile fail normal enabled 'did not all reach both fields'
use degraded
extra_args=(--tolerate-unready '[{"key":"StatefulSet/other","generation":3,"uid":"u","ready":1}]')
check 'a tolerated record naming an unreviewed template is refused' after-reconcile fail normal enabled 'record is malformed'
extra_args=(--tolerate-unready 'not-json')
check 'a tolerated record that is not JSON is refused' after-reconcile fail normal enabled 'record is malformed'
extra_args=()

printf 'PASS: Coroot baseline guard fixtures\n'
