#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
policy="$root/k8s/providers/hetzner/infrastructure/cluster-policies/restrict-storage-to-baseline-workers.yaml"
retrofit="$root/k8s/providers/hetzner/infrastructure/cluster-policies/drain-autoscale-node-storage.yaml"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

kyverno apply "$policy" --resource "$root/tests/longhorn-migration-replica-exclusion/resources.yaml" --output "$work" --remove-color >"$work/apply.log" 2>&1 || {
  cat "$work/apply.log"
  exit 1
}
found=0
for file in "$work"/*.yaml; do
  [[ -f "$file" ]] || continue
  found=1
  yq -o=json -I=0 ea 'select(.kind == "Node")' "$file" >>"$work/nodes.jsonl"
done
[[ "$found" == 1 ]]
jq -s -e '
  def node($name): [.[]|select(.metadata.name==$name)]|if length==1 then .[0] else error("missing or duplicate fixture") end;
  def fenced($name): node($name)|.spec.allowScheduling==false and .spec.evictionRequested==true;
  def allowed($name): node($name)|.spec.allowScheduling==true and .spec.evictionRequested==false;
  fenced("autoscale-cx43-0123456789abcdef") and
  fenced("autoscale-cx43-1123456789abcdef") and
  fenced("autoscale-longhorn-v1-0123456789abcdef") and
  fenced("autoscale-longhorn-v10-0123456789abcdef") and
  allowed("autoscale-longhorn-v1-1123456789abcdef") and
  allowed("autoscale-longhorn-v2-2123456789abcdef") and
  allowed("prod-worker-1")
' "$work/nodes.jsonl" >/dev/null || {
  echo 'replica exclusion did not distinguish protected migration capacity from ordinary compute' >&2
  exit 1
}

# A compute-node trigger must never retrofit every migration node as a side effect.
[[ $(yq -r '.spec.rules[0].mutate.targets[0].name' "$retrofit") == '{{ request.object.metadata.name }}' ]]
cmp <(yq -o=json '.spec.rules[0].preconditions' "$policy") <(yq -o=json '.spec.rules[0].preconditions' "$retrofit")

# Exercise the native mutate-existing target resolver with an ordinary compute
# trigger and protected migration nodes present in the same fake API inventory.
yq 'select(.metadata.name == "autoscale-cx43-0123456789abcdef")' \
  "$root/tests/longhorn-migration-replica-exclusion/resources.yaml" >"$work/trigger.yaml"
yq 'select(.metadata.name != "autoscale-cx43-0123456789abcdef")' \
  "$root/tests/longhorn-migration-replica-exclusion/resources.yaml" >"$work/targets.yaml"
mkdir "$work/retrofit"
kyverno apply "$retrofit" --resource "$work/trigger.yaml" --target-resource "$work/targets.yaml" \
  --output "$work/retrofit" --remove-color >"$work/retrofit.log" 2>&1 || {
  cat "$work/retrofit.log"
  exit 1
}
for file in "$work/retrofit"/*.yaml; do
  [[ -f "$file" ]] || continue
  yq -o=json -I=0 ea 'select(.kind == "Node")' "$file" >>"$work/retrofit.jsonl"
done
[[ -s "$work/retrofit.jsonl" ]]
jq -s -e 'length > 0 and all(.[]; .metadata.name == "autoscale-cx43-0123456789abcdef") and
  any(.[]; .spec.allowScheduling == false and .spec.evictionRequested == true)' "$work/retrofit.jsonl" >/dev/null || {
  echo 'compute trigger changed another node or did not apply its own fence' >&2
  exit 1
}
echo 'PASS: only armed, exactly named migration pools escape compute-node exclusion; retrofit targets stay bound to their trigger'
