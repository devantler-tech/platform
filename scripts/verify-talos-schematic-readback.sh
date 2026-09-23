#!/usr/bin/env bash
set -euo pipefail

# The kernel arguments are baked into the Talos Image Factory image. A green
# machine-config sync does not prove that already-running nodes booted it.
root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
config="$root_dir/ksail.prod.yaml"
schematic="$root_dir/talos/factory-schematic.yaml"
kubectl_bin=${KUBECTL_BIN:-kubectl}

if command -v sha256sum >/dev/null 2>&1; then
  expected_schematic=$(sha256sum "$schematic" | awk '{print $1}')
else
  expected_schematic=$(shasum -a 256 "$schematic" | awk '{print $1}')
fi
expected_version=$(yq eval '.spec.cluster.talos.version' "$config")
expected_control_planes=$(yq eval '.spec.cluster.controlPlanes' "$config")
expected_workers=$(yq eval '.spec.cluster.workers' "$config")

[[ "$expected_schematic" =~ ^[0-9a-f]{64}$ &&
   "$expected_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ &&
   "$expected_control_planes" =~ ^[1-9][0-9]*$ &&
   "$expected_workers" =~ ^[1-9][0-9]*$ ]] || {
  printf 'invalid desired Talos version, schematic, or static-node count\n' >&2
  exit 1
}

if [[ -n "${KUBE_CONTEXT:-}" ]]; then
  nodes_json=$("$kubectl_bin" --context "$KUBE_CONTEXT" get nodes -o json)
else
  nodes_json=$("$kubectl_bin" get nodes -o json)
fi
if ! jq -e \
  --arg schematic "$expected_schematic" \
  --arg os_image "Talos ($expected_version)" \
  --argjson control_planes "$expected_control_planes" \
  --argjson workers "$expected_workers" '
    (.items | type) == "array" and
    ([.items[] | select(.metadata.name | startswith("prod-control-plane-"))] | length) == $control_planes and
    ([.items[] | select(.metadata.name | startswith("prod-worker-"))] | length) == $workers and
    all(.items[];
      .metadata.annotations["extensions.talos.dev/schematic"] == $schematic and
      .status.nodeInfo.osImage == $os_image and
      (.spec.unschedulable != true) and
      any(.status.conditions[]; .type == "Ready" and .status == "True")
    )
  ' <<<"$nodes_json" >/dev/null; then
  printf 'Talos production node readback differs from desired version %s and schematic %s\n' \
    "$expected_version" "$expected_schematic" >&2
  jq -r '.items[]? | [.metadata.name, (.status.nodeInfo.osImage // "missing-version"),
    (.metadata.annotations["extensions.talos.dev/schematic"] // "missing-schematic"),
    (if .spec.unschedulable == true then "cordoned" else "schedulable" end),
    (if any(.status.conditions[]?; .type == "Ready" and .status == "True") then "Ready" else "NotReady" end)] | @tsv' \
    <<<"$nodes_json" >&2
  exit 1
fi

printf 'PASS: all Talos nodes run %s with schematic %s and are Ready/schedulable\n' \
  "$expected_version" "$expected_schematic"
