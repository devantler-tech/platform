#!/usr/bin/env bash
# Admission write for the Coroot-scoped baseline-context mutation.
# The baseline-context-coroot rules act on CREATE and UPDATE only, and the
# Coroot operator writes its six templates only when its own desired state
# changes. So a template that lacks either field gets one metadata-only write:
# an annotation, which sends the object through admission so the rules add
# fsGroupChangePolicy and an empty seLinuxOptions. The changed template then
# rolls its pods, and this step waits for each rollout to finish.
# Templates that already carry both fields are not written, so a deploy after
# activation restarts nothing. guard-coroot-baseline-context.sh after-reconcile
# runs next and proves the operator does not rewrite the templates in a loop.
set -euo pipefail
context=admin@prod
rollout_timeout=15m
while [[ $# -gt 0 ]]; do
  case "$1" in
    --context) context="${2:?context required}"; shift 2 ;;
    --rollout-timeout) rollout_timeout="${2:?timeout required}"; shift 2 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
fail() { printf '::error::Coroot baseline admission: %s\n' "$1" >&2; exit 1; }

# The reviewed population, identical to guard-coroot-baseline-context.sh. Every
# read must match it exactly, so no write ever reaches a template outside it.
expected='["DaemonSet/coroot-node-agent","Deployment/coroot-cluster-agent","Deployment/coroot-prometheus","StatefulSet/coroot-clickhouse-keeper","StatefulSet/coroot-clickhouse-shard-0","StatefulSet/coroot-coroot"]'

read_templates() {
  kubectl --context "${context}" get deployments,statefulsets,daemonsets --namespace observability \
    --selector app.kubernetes.io/managed-by=coroot-operator -o json --request-timeout=20s \
    >"${scratch}/templates.json" || fail 'API read failed'
  jq -e '.items | type == "array" and length > 0' "${scratch}/templates.json" >/dev/null ||
    fail 'API returned no Coroot templates'
  jq -e --argjson expected "${expected}" \
    '[.items[] | "\(.kind)/\(.metadata.name)"] | sort == $expected' "${scratch}/templates.json" >/dev/null ||
    fail 'the Coroot-generated templates differ from the reviewed six'
}
# Same object-presence predicate as the guard: pod-level fsGroupChangePolicy,
# and seLinuxOptions at pod level or on every container and init container.
lacking() {
  jq -r '.items[] | select(.spec.template.spec as $pod |
    ((($pod.securityContext // {}) | has("fsGroupChangePolicy")) and
    ((($pod.securityContext // {}) | has("seLinuxOptions")) or
      ([($pod.containers + ($pod.initContainers // []))[] | (.securityContext // {}) | has("seLinuxOptions")] | all))) | not)
    | "\(.kind | ascii_downcase)/\(.metadata.name)"' "${scratch}/templates.json"
}

read_templates
lacking >"${scratch}/targets"
if [[ ! -s "${scratch}/targets" ]]; then
  printf 'No admission write needed: every Coroot template carries both fields\n'
  exit 0
fi

# A write returns only after admission ran, so a template still lacking a field
# was admitted before the policy engine saw the namespace label Flux just
# applied. Write those again, a bounded number of times. Every template written
# in any round is rolled out, including one that only starts lacking a field
# during a retry.
cp "${scratch}/targets" "${scratch}/still"
cp "${scratch}/targets" "${scratch}/written"
for ((attempt = 1; attempt <= 3; attempt++)); do
  stamp="${GITHUB_RUN_ID:-manual}-${GITHUB_RUN_ATTEMPT:-1}-${attempt}"
  while IFS= read -r target; do
    kubectl --context "${context}" annotate --namespace observability --overwrite "${target}" \
      "pod-security.devantler.tech/baseline-context-coroot-admitted=${stamp}" --request-timeout=20s \
      >/dev/null || fail "admission write failed for ${target}"
    printf 'Wrote %s through admission\n' "${target}"
  done <"${scratch}/still"
  read_templates
  lacking >"${scratch}/still"
  [[ -s "${scratch}/still" ]] || break
  sort -u "${scratch}/written" "${scratch}/still" -o "${scratch}/written"
  if (( attempt < 3 )); then sleep 10; fi
done
[[ ! -s "${scratch}/still" ]] ||
  fail "admission did not add both fields to: $(paste -sd ' ' "${scratch}/still")"

while IFS= read -r target; do
  kubectl --context "${context}" rollout status --namespace observability "${target}" \
    --timeout="${rollout_timeout}" >/dev/null || fail "rollout of ${target} did not finish within ${rollout_timeout}"
  printf 'Rolled out %s\n' "${target}"
done <"${scratch}/written"
