#!/usr/bin/env bash
# Read-only convergence guard for the Coroot-scoped baseline-context mutation.
# The `baseline-context-coroot` namespace label lets admission add
# fsGroupChangePolicy and empty seLinuxOptions to the six Coroot-operator
# templates. The operator did not set those fields, so it may rewrite its
# objects to remove them and loop against admission. While the label is
# declared, every deployment re-proves that the six stored templates carry
# both fields, are ready, and are not being rewritten. An operator upgrade can
# change its desired state at any time, so a past proof is never reused.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
phase="${1:-}"
[[ $# -gt 0 ]] && shift
context=admin@prod
manifest="${root_dir}/k8s/bases/infrastructure/controllers/coroot/namespace.yaml"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --context) context="${2:?context required}"; shift 2 ;;
    --namespace-manifest) manifest="${2:?namespace manifest required}"; shift 2 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
case "${phase}" in before-publish|after-reconcile) ;; *) exit 2 ;; esac
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
fail() { printf '::error::Coroot baseline context: %s\n' "$1" >&2; exit 1; }
output() {
  printf 'rollout_required=%s\n' "$1"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then printf 'rollout_required=%s\n' "$1" >>"${GITHUB_OUTPUT}"; fi
}

# The reviewed population. A seventh or a missing template means the operator
# changed shape, and the reviewed scope no longer describes what admission reaches.
expected='["DaemonSet/coroot-node-agent","Deployment/coroot-cluster-agent","Deployment/coroot-prometheus","StatefulSet/coroot-clickhouse-keeper","StatefulSet/coroot-clickhouse-shard-0","StatefulSet/coroot-coroot"]'

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
owned_and_ready() {
  jq -e 'all(.items[];
    .metadata.namespace == "observability" and
    any((.metadata.ownerReferences // [])[]; .kind == "Coroot" and .controller == true) and
    .status.observedGeneration == .metadata.generation and
    (if .kind == "DaemonSet" then
      .status.desiredNumberScheduled > 0 and
      .status.numberReady == .status.desiredNumberScheduled and
      .status.updatedNumberScheduled == .status.desiredNumberScheduled and
      (.status.numberUnavailable // 0) == 0
    else
      (.spec.replicas // 1) as $want |
      .status.readyReplicas == $want and .status.updatedReplicas == $want and
      (.status.availableReplicas // $want) == $want and (.status.replicas // $want) == $want
    end))' "${scratch}/templates.json" >/dev/null
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

if [[ "${phase}" == before-publish ]]; then
  # A reverted label must not depend on the operator's workloads to recover.
  if ! configured="$(yq -r '.metadata.labels["pod-security.devantler.tech/baseline-context-coroot"] // ""' "${manifest}")"; then
    fail 'desired namespace manifest is malformed'
  fi
  case "${configured}" in
    enabled) ;;
    '') output false; exit 0 ;;
    *) fail "unexpected baseline-context-coroot value '${configured}'" ;;
  esac
  read_templates
  population || fail 'the Coroot-generated templates differ from the reviewed six'
  owned_and_ready || fail 'a Coroot template is not operator-owned or not fully ready'
  output true
  exit 0
fi

# Flux has reported the released revision Ready. Allow bounded rollout time,
# then require consecutive stable observations of all six stored templates.
ready=false
for ((attempt = 0; attempt < 12; attempt++)); do
  read_templates
  if population && owned_and_ready && has_fields; then
    ready=true
    break
  fi
  sleep 5
done
[[ "${ready}" == true ]] || fail 'the six templates did not all reach both fields and a complete healthy rollout'
baseline="$(snapshot)"
previous="${baseline}"
# Per template, so one write each on two objects is not mistaken for a loop.
writes='{}'
for ((sample = 0; sample < 3; sample++)); do
  sleep 10
  read_templates
  population || fail 'the Coroot template population changed during observation'
  owned_and_ready || fail 'a Coroot template lost readiness during observation'
  has_fields || fail 'the operator removed an admitted field during observation'
  current="$(snapshot)"
  jq -e -n --argjson a "${baseline}" --argjson b "${current}" \
    '[$a[] | {key, generation, uid}] == [$b[] | {key, generation, uid}]' >/dev/null ||
    fail 'the operator rewrote or replaced a template during observation'
  writes="$(jq -c -n --argjson a "${previous}" --argjson b "${current}" --argjson w "${writes}" \
    'reduce range($a | length) as $i ($w;
      if $a[$i].version != $b[$i].version then .[$a[$i].key] += 1 else . end)')"
  previous="${current}"
  jq -e 'all(.[]; . < 2)' <<<"${writes}" >/dev/null ||
    fail "repeated owner writes continued during observation: ${writes}"
done
printf 'PASS: six Coroot templates carry both fields, are ready, and are stable\n'
