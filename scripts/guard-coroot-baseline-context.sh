#!/usr/bin/env bash
# Read-only convergence guard for the Coroot-scoped baseline-context mutation.
# The `baseline-context-coroot` namespace label lets admission add
# fsGroupChangePolicy and empty seLinuxOptions to the six Coroot-operator
# templates. The operator did not set those fields, so it may rewrite its
# objects to remove them and loop against admission. While the label is
# declared, every deployment re-proves that the six stored templates carry
# both fields, are ready, and are not being rewritten. An operator upgrade can
# change its desired state at any time, so a past proof is never reused.
#
# Readiness alone is not evidence of that loop: a Coroot fault unrelated to
# admission also leaves a template unready, and refusing every deployment for
# it would also refuse the deployment that repairs production. A template that
# was already unready before publication is therefore tolerated, but only when
# all six templates already carry both fields and stay unrewritten over an
# observation window, and only for as long as that template's generation, UID
# and ready count do not get worse. It is reported as its own result, never as
# a pass.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
phase="${1:-}"
[[ $# -gt 0 ]] && shift
context=admin@prod
manifest="${root_dir}/k8s/bases/infrastructure/controllers/coroot/namespace.yaml"
tolerated='[]'
while [[ $# -gt 0 ]]; do
  case "$1" in
    --context) context="${2:?context required}"; shift 2 ;;
    --namespace-manifest) manifest="${2:?namespace manifest required}"; shift 2 ;;
    --tolerate-unready) tolerated="${2:?tolerated record required}"; shift 2 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
case "${phase}" in before-publish|after-reconcile) ;; *) exit 2 ;; esac
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
fail() { printf '::error::Coroot baseline context: %s\n' "$1" >&2; exit 1; }
output() {
  printf '%s=%s\n' "$1" "$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then printf '%s=%s\n' "$1" "$2" >>"${GITHUB_OUTPUT}"; fi
}

# The reviewed population. A seventh or a missing template means the operator
# changed shape, and the reviewed scope no longer describes what admission reaches.
expected='["DaemonSet/coroot-node-agent","Deployment/coroot-cluster-agent","Deployment/coroot-prometheus","StatefulSet/coroot-clickhouse-keeper","StatefulSet/coroot-clickhouse-shard-0","StatefulSet/coroot-coroot"]'

# Shared jq definitions: one readiness predicate for every phase.
# shellcheck disable=SC2016 # jq variables, expanded by jq
defs='
  def key: "\(.kind)/\(.metadata.name)";
  def ready_count: if .kind == "DaemonSet" then (.status.numberReady // 0) else (.status.readyReplicas // 0) end;
  def ready:
    if .kind == "DaemonSet" then
      .status.desiredNumberScheduled > 0 and
      .status.numberReady == .status.desiredNumberScheduled and
      .status.updatedNumberScheduled == .status.desiredNumberScheduled and
      (.status.numberUnavailable // 0) == 0
    else
      (.spec.replicas // 1) as $want |
      .status.readyReplicas == $want and .status.updatedReplicas == $want and
      (.status.availableReplicas // $want) == $want and (.status.replicas // $want) == $want
    end;
  # A tolerated template is the same object at the same generation, no less ready than recorded.
  def tolerated($records): . as $item |
    any($records[]; .key == ($item | key) and .generation == $item.metadata.generation and
      .uid == $item.metadata.uid and ($item | ready_count) >= .ready);'

read_templates() {
  kubectl --context "${context}" get deployments,statefulsets,daemonsets --namespace observability \
    --selector app.kubernetes.io/managed-by=coroot-operator -o json --request-timeout=20s \
    >"${scratch}/templates.json" || fail 'API read failed'
  jq -e '.items | type == "array"' "${scratch}/templates.json" >/dev/null || fail 'API returned no item list'
}
population() {
  jq -e --argjson expected "${expected}" \
    '[.items[] | "\(.kind)/\(.metadata.name)"] | sort == $expected' "${scratch}/templates.json" >/dev/null
}
owned() {
  jq -e 'all(.items[];
    .metadata.namespace == "observability" and
    any((.metadata.ownerReferences // [])[]; .kind == "Coroot" and .controller == true))' \
    "${scratch}/templates.json" >/dev/null
}
observed() {
  jq -e 'all(.items[]; .status.observedGeneration == .metadata.generation)' "${scratch}/templates.json" >/dev/null
}
# Every template is ready, or is a tolerated pre-existing unready one.
ready_except() {
  jq -e --argjson records "$1" "${defs}"'all(.items[]; ready or tolerated($records))' \
    "${scratch}/templates.json" >/dev/null
}
unready_records() {
  jq -c "${defs}"'[.items[] | select(ready | not) |
    {key: key, generation: .metadata.generation, uid: .metadata.uid, ready: ready_count}] | sort_by(.key)' \
    "${scratch}/templates.json"
}
# The C-0211 object-presence predicate: pod-level fsGroupChangePolicy, and
# seLinuxOptions at pod level or on every container and init container.
has_fields() {
  jq -e 'all(.items[]; .spec.template.spec as $pod |
    (($pod.securityContext // {}) | has("fsGroupChangePolicy")) and
    ((($pod.securityContext // {}) | has("seLinuxOptions")) or
      ([($pod.containers + ($pod.initContainers // []))[] | (.securityContext // {}) | has("seLinuxOptions")] | all)))' \
    "${scratch}/templates.json" >/dev/null
}
snapshot() {
  jq -c '[.items[] | {key: "\(.kind)/\(.metadata.name)", generation: .metadata.generation,
    version: .metadata.resourceVersion, uid: .metadata.uid}] | sort_by(.key)' "${scratch}/templates.json"
}
# Three samples over thirty seconds. A changed generation or UID, a removed
# field, or two writes to one template is the operator working against
# admission. $1 names the readiness check each sample must also satisfy.
observe_stability() {
  local readiness="$1" baseline previous current writes='{}' sample
  baseline="$(snapshot)"
  previous="${baseline}"
  for ((sample = 0; sample < 3; sample++)); do
    sleep 10
    read_templates
    population || fail 'the Coroot template population changed during observation'
    owned || fail 'a Coroot template lost its operator owner during observation'
    current="$(snapshot)"
    jq -e -n --argjson a "${baseline}" --argjson b "${current}" \
      '[$a[] | {key, generation, uid}] == [$b[] | {key, generation, uid}]' >/dev/null ||
      fail 'the operator rewrote or replaced a template during observation'
    # After the rewrite check: a rewritten template also voids its tolerated record.
    if ! observed || ! "${readiness}"; then fail 'a Coroot template lost readiness during observation'; fi
    has_fields || fail 'the operator removed an admitted field during observation'
    # Per template, so one write each on two objects is not mistaken for a loop.
    writes="$(jq -c -n --argjson a "${previous}" --argjson b "${current}" --argjson w "${writes}" \
      'reduce range($a | length) as $i ($w;
        if $a[$i].version != $b[$i].version then .[$a[$i].key] += 1 else . end)')"
    previous="${current}"
    jq -e 'all(.[]; . < 2)' <<<"${writes}" >/dev/null ||
      fail "repeated owner writes continued during observation: ${writes}"
  done
}
ready_or_tolerated() { ready_except "${tolerated}"; }

if [[ "${phase}" == before-publish ]]; then
  # A reverted label must not depend on the operator's workloads to recover.
  if ! configured="$(yq -r '.metadata.labels["pod-security.devantler.tech/baseline-context-coroot"] // ""' "${manifest}")"; then
    fail 'desired namespace manifest is malformed'
  fi
  case "${configured}" in
    enabled) ;;
    '') output rollout_required false; exit 0 ;;
    *) fail "unexpected baseline-context-coroot value '${configured}'" ;;
  esac
  read_templates
  population || fail 'the Coroot-generated templates differ from the reviewed six'
  owned || fail 'a Coroot template is not operator-owned'
  if observed && ready_except '[]'; then
    output rollout_required true
    exit 0
  fi
  # Unready before anything is published. Without both fields on all six this
  # cannot be told apart from an admission rollout, so it stays a refusal.
  has_fields ||
    fail 'a Coroot template is not fully ready and the admitted fields are not all present'
  observed || fail 'a Coroot template is not fully ready: its controller has not observed the latest generation'
  # Only what was unready at the first read is excused: a template that loses
  # readiness while being observed is a fault still spreading, not a settled one.
  tolerated="$(unready_records)"
  observe_stability ready_or_tolerated
  records="$(unready_records)"
  output rollout_required true
  if [[ "${records}" != '[]' ]]; then
    output preexisting_unready "${records}"
    printf '::warning::Coroot baseline context: already unready before this deployment, with both fields present and no rewrite: %s\n' \
      "$(jq -r 'map(.key) | join(", ")' <<<"${records}")"
  fi
  exit 0
fi

jq -e --argjson expected "${expected}" 'type == "array" and all(.[];
  (.key | IN($expected[])) and (.generation | type == "number") and (.uid | type == "string") and
  (.ready | type == "number"))' <<<"${tolerated}" >/dev/null 2>&1 ||
  fail 'the tolerated pre-existing unready record is malformed'

# Flux can report the Coroot resource Ready before its generated workloads finish
# rolling. In particular, node agents update sequentially. Allow ten minutes of
# elapsed convergence time (including API reads), then require the same stable
# observations of all six templates. A final in-flight API read remains bounded
# by its request timeout; unsuccessful convergence never passes the guard.
ready=false
deadline=$((SECONDS + 600))
while ((SECONDS < deadline)); do
  read_templates
  if population && owned && observed && ready_or_tolerated && has_fields; then
    ready=true
    break
  fi
  sleep 5
done
[[ "${ready}" == true ]] || fail 'the six templates did not all reach both fields and a complete healthy rollout within ten minutes'
observe_stability ready_or_tolerated
still="$(unready_records)"
if [[ "${still}" != '[]' ]]; then
  # Reached only through the tolerated record: never a pass, and never silent.
  output baseline_result preexisting-unready
  printf '::warning::Coroot baseline context: still unready, as before this deployment: %s\n' \
    "$(jq -r 'map(.key) | join(", ")' <<<"${still}")"
  printf 'PREEXISTING-UNREADY: six Coroot templates carry both fields and are stable; %s was already unready before this deployment and still is\n' \
    "$(jq -r 'map(.key) | join(", ")' <<<"${still}")"
  exit 0
fi
output baseline_result pass
printf 'PASS: six Coroot templates carry both fields, are ready, and are stable\n'
