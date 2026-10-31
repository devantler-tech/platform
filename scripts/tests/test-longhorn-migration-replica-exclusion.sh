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
# The retrofit judges the live target with the admission rule's own condition, and
# nothing at rule level can act on the trigger as it was queued.
[[ $(yq -r '.spec.rules[0].preconditions' "$retrofit") == null ]]
cmp <(yq -o=json '.spec.rules[0].preconditions' "$policy" | sed 's/request\.object\./target./g') \
  <(yq -o=json '.spec.rules[0].mutate.targets[0].preconditions' "$retrofit")

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

# A delayed update can carry a trigger that predates the live Node's protection.
# The CLI cannot hold two copies of one Node, so a test copy of the rule aims an
# unprotected trigger at a protected Node: only a condition that reads the target
# can refuse it. The control aims the same trigger at an unprotected Node.
resources="$root/tests/longhorn-migration-replica-exclusion/resources.yaml"
fence_from_stale_trigger() {
  local target=$1 out=$2
  yq "(.spec.rules[0].mutate.targets[0].name) = \"$target\"" "$retrofit" >"$out.policy.yaml"
  yq "select(.metadata.name == \"$target\")" "$resources" >"$out.target.yaml"
  [[ -s "$out.target.yaml" ]]
  mkdir "$out"
  kyverno apply "$out.policy.yaml" --resource "$work/trigger.yaml" --target-resource "$out.target.yaml" \
    --output "$out" --remove-color >"$out.log" 2>&1 || {
    cat "$out.log"
    exit 1
  }
  grep -rqs 'evictionRequested: true' "$out"
}
if fence_from_stale_trigger autoscale-longhorn-v1-1123456789abcdef "$work/stale-protected"; then
  echo 'a stale trigger fenced a migration node whose live copy is enabled' >&2
  exit 1
fi
fence_from_stale_trigger autoscale-cx43-1123456789abcdef "$work/stale-unprotected" || {
  echo 'the stale-trigger control applied no fence, so the check above proves nothing' >&2
  exit 1
}
echo 'PASS: only armed, exactly named migration pools escape compute-node exclusion; retrofit targets stay bound to their trigger'
