#!/usr/bin/env bash
# Main-only production workflow operation. No workload, namespace or storage
# deletion is permitted. Assess is read-only; apply deletes only legacy generated
# VPAs whose target is proven absent twice, using UID/RV delete preconditions.
# All API errors are private; public logs contain aggregate counts only.
set -euo pipefail
umask 077
fail() { printf 'REFUSED: %s\n' "$1" >&2; exit 1; }
unknown() { printf 'UNKNOWN: %s\n' "$1" >&2; exit 2; }
mode="${1:-assess}"
[[ "$#" -le 1 && ( "$mode" == assess || "$mode" == apply ) ]] || fail 'invalid operation'
[[ "${GITHUB_REPOSITORY:-}" == devantler-tech/platform &&
   "${GITHUB_EVENT_NAME:-}" == workflow_dispatch &&
   "${GITHUB_REF:-}" == refs/heads/main &&
   "${GITHUB_RUN_ATTEMPT:-}" == 1 &&
   "${VPA_CLEANUP_CONFIRM:-}" == cleanup-orphan-generated-vpas ]] ||
  fail 'requires the confirmed first-attempt main workflow'
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
k() { kubectl --context admin@prod --request-timeout=10s "$@" 2>/dev/null; }
readonly resource=verticalpodautoscalers.autoscaling.k8s.io

# Do not clean up before the reviewed prevention is deployed. Compare the
# complete generator, including the required real UID expression, to Git.
if [[ "$mode" == apply ]]; then
  for kind in deployment statefulset daemonset; do
    yq -o=json '.spec.generate' "$root/k8s/bases/infrastructure/cluster-policies/best-practices/auto-vpa-$kind.yaml" >"$work/expected.json" 2>/dev/null ||
      unknown 'cannot read the reviewed generator'
    k get generatingpolicies.policies.kyverno.io "auto-vpa-$kind" -o json >"$work/policy.json" ||
      unknown 'cannot read the deployed generator'
    jq -e --slurpfile expected "$work/expected.json" '.spec.generate == $expected[0]' "$work/policy.json" >/dev/null 2>&1 ||
      fail 'the reviewed prevention has not deployed'
  done
fi
k get "$resource" --all-namespaces -o json >"$work/list.json" || unknown 'cannot list autoscalers'
jq -e '.items | type=="array" and length<=250' "$work/list.json" >/dev/null 2>&1 ||
  unknown 'autoscaler census is malformed or exceeds its bound'
jq -e '(.metadata.continue // "") == ""' "$work/list.json" >/dev/null 2>&1 ||
  unknown 'autoscaler census is incomplete'
jq -c '.items[] | select(.metadata.labels["generate.kyverno.io/policy-name"] |
  .=="auto-vpa-deployment" or .=="auto-vpa-statefulset" or .=="auto-vpa-daemonset")' "$work/list.json" >"$work/generated.jsonl" ||
  unknown 'cannot classify the census'

# A named get that fails is UNKNOWN. Only a successful empty ignore-not-found
# response proves absence; every existing incarnation is retained.
target_absent() {
  local obj="$1" out ns name kind target_resource
  ns="$(jq -r '.metadata.namespace' <<<"$obj")"
  name="$(jq -r '.spec.targetRef.name' <<<"$obj")"
  kind="$(jq -r '.spec.targetRef.kind' <<<"$obj")"
  case "$kind" in
    Deployment) target_resource=deployments.apps ;;
    StatefulSet) target_resource=statefulsets.apps ;;
    DaemonSet) target_resource=daemonsets.apps ;;
    *) fail 'unexpected target kind' ;;
  esac
  out="$(k get "$target_resource" "$name" -n "$ns" --ignore-not-found -o json)" ||
    unknown 'cannot read a target'
  [[ -n "$out" ]] || return 0
  jq -e --arg ns "$ns" --arg name "$name" --arg kind "$kind" '
    .apiVersion=="apps/v1" and .kind==$kind and .metadata.namespace==$ns and
    .metadata.name==$name and (.metadata.uid | type=="string" and length>0)' <<<"$out" >/dev/null 2>&1 ||
    unknown 'target read is malformed'
  return 1
}

generated=0; retained=0; candidates=0; deleted=0
: >"$work/plan.jsonl"
while IFS= read -r obj; do
  [[ -n "$obj" ]] || continue
  generated=$((generated + 1))
  jq -e '
    def dns: type=="string" and length>0 and length<=253 and test("^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$");
    def uuid: type=="string" and test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$");
    .metadata as $m | $m.labels as $l | .spec.targetRef as $t |
    .apiVersion=="autoscaling.k8s.io/v1" and .kind=="VerticalPodAutoscaler" and
    ($m.name|dns) and ($m.namespace|dns) and ($m.uid|uuid) and
    ($m.resourceVersion|type=="string" and test("^[0-9]+$")) and
    (($m.ownerReferences // []) | type=="array") and
    $l["app.kubernetes.io/managed-by"]=="kyverno" and
    $l["generate.kyverno.io/trigger-group"]=="apps" and
    $l["generate.kyverno.io/trigger-version"]=="v1" and
    ($l["generate.kyverno.io/trigger-uid"]|uuid) and
    $t.apiVersion=="apps/v1" and
    ($t.kind=="Deployment" or $t.kind=="StatefulSet" or $t.kind=="DaemonSet") and
    ($t.name|dns) and $t.name==$l["generate.kyverno.io/trigger-name"] and
    $t.kind==$l["generate.kyverno.io/trigger-kind"] and
    $m.namespace==$l["generate.kyverno.io/trigger-namespace"] and
    $l["generate.kyverno.io/policy-name"]==("auto-vpa-"+($t.kind|ascii_downcase)) and
    $m.name==($t.name+"-"+($t.kind|ascii_downcase))' <<<"$obj" >/dev/null 2>&1 ||
    fail 'generated autoscaler provenance is inconsistent'
  # Owner-backed VPAs belong to Kubernetes GC; finalizers/deletion are not
  # overridden. The cleanup is solely for the old ownerless generation.
  if ! jq -e '(.metadata.ownerReferences // [] | length)==0 and
    (.metadata.finalizers // [] | length)==0 and .metadata.deletionTimestamp==null' <<<"$obj" >/dev/null; then
    retained=$((retained + 1)); continue
  fi
  if target_absent "$obj"; then
    printf '%s\n' "$obj" >>"$work/plan.jsonl"
    candidates=$((candidates + 1))
  else
    retained=$((retained + 1))
  fi
done <"$work/generated.jsonl"
# No deletion occurs until the entire census and every initial target read have
# succeeded. Re-read identity and target immediately before each bounded delete.
if [[ "$mode" == apply ]]; then
  while IFS= read -r obj; do
    [[ -n "$obj" ]] || continue
    ns="$(jq -r '.metadata.namespace' <<<"$obj")"
    name="$(jq -r '.metadata.name' <<<"$obj")"
    current="$(k get "$resource" "$name" -n "$ns" --ignore-not-found -o json)" ||
      unknown 'cannot re-read an autoscaler'
    [[ -n "$current" ]] || continue
    jq -e --argjson old "$obj" '.metadata.uid==$old.metadata.uid and
      .metadata.resourceVersion==$old.metadata.resourceVersion' <<<"$current" >/dev/null 2>&1 ||
      fail 'autoscaler changed after assessment'
    target_absent "$obj" || fail 'a target returned after assessment'
    jq '{apiVersion:"v1",kind:"DeleteOptions",preconditions:{
      uid:.metadata.uid,resourceVersion:.metadata.resourceVersion}}' <<<"$obj" >"$work/delete.json"
    k delete --raw "/apis/autoscaling.k8s.io/v1/namespaces/$ns/verticalpodautoscalers/$name" \
      --filename "$work/delete.json" >/dev/null || unknown 'preconditioned deletion failed'
    remaining="$(k get "$resource" "$name" -n "$ns" --ignore-not-found -o json)" ||
      unknown 'cannot verify deletion'
    [[ -z "$remaining" ]] || fail 'an autoscaler remains after deletion'
    deleted=$((deleted + 1))
  done <"$work/plan.jsonl"
fi
printf 'OK: mode=%s generated=%s retained=%s absent-target-candidates=%s deleted=%s\n' \
  "$mode" "$generated" "$retained" "$candidates" "$deleted"
