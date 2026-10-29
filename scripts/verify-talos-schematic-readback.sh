#!/usr/bin/env bash
set -euo pipefail

# The kernel arguments are baked into the Talos Image Factory image. A green
# machine-config sync does not prove that already-running nodes booted it.
root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
config="$root_dir/ksail.prod.yaml"
schematic="$root_dir/talos/factory-schematic.yaml"
kubectl_bin=${KUBECTL_BIN:-kubectl}
# Deployments restore a kubeconfig that contains admin@prod, but its current
# context is not a trusted cluster selector. Operators can explicitly supply
# an OIDC context for read-only diagnosis.
kube_context=${KUBE_CONTEXT:-admin@prod}
max_attempts=${TALOS_READBACK_ATTEMPTS:-60}
interval_seconds=${TALOS_READBACK_INTERVAL_SECONDS:-10}
max_seconds=${TALOS_READBACK_MAX_SECONDS:-600}

[[ "$max_attempts" =~ ^[1-9][0-9]*$ &&
   "$interval_seconds" =~ ^(0|[1-9][0-9]*)$ &&
   "$max_seconds" =~ ^[1-9][0-9]*$ ]] || {
  printf 'invalid Talos readback attempt, interval, or deadline setting\n' >&2
  exit 1
}

if command -v sha256sum >/dev/null 2>&1; then
  expected_schematic=$(sha256sum "$schematic" | awk '{print $1}')
else
  expected_schematic=$(shasum -a 256 "$schematic" | awk '{print $1}')
fi
expected_version=$(yq eval '.spec.cluster.talos.version' "$config")
expected_control_planes=$(yq eval '.spec.cluster.controlPlanes' "$config")
expected_workers=$(yq eval '.spec.cluster.workers' "$config")
expected_migration_pools=$(yq -o=json '.spec.cluster.autoscaler.node.pools' "$config" | jq '
  [ .[] | select(.name | startswith("autoscale-longhorn-")) |
    {name, min, max, serverType, role: .labels["storage.platform.devantler.tech/migration-pool"]} ]')

# The V2 migration pool replaces the ordinary warm spare. Allow its native
# ten-minute unneeded timer, graceful removal and cold boot to finish. Explicit
# diagnostic limits still win, and every required worker must actually be Ready.
if jq -e 'any(.[]; .role == "v2" and .min > 0)' <<<"$expected_migration_pools" >/dev/null; then
  max_attempts=${TALOS_READBACK_ATTEMPTS:-120}
  max_seconds=${TALOS_READBACK_MAX_SECONDS:-1200}
fi

[[ "$expected_schematic" =~ ^[0-9a-f]{64}$ &&
   "$expected_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ &&
   "$expected_control_planes" =~ ^[1-9][0-9]*$ &&
   "$expected_workers" =~ ^[1-9][0-9]*$ ]] || {
  printf 'invalid desired Talos version, schematic, or static-node count\n' >&2
  exit 1
}

jq -e 'all(.[]; (.name | test("^autoscale-longhorn-v[12]$")) and
  (.role == "v1" or .role == "v2") and .name == ("autoscale-longhorn-" + .role) and
  (.min | type) == "number" and .min >= 0 and (.max | type) == "number" and .max >= .min and
  (.serverType | type) == "string" and .serverType != "")' <<<"$expected_migration_pools" >/dev/null || {
  printf 'invalid desired migration pool configuration\n' >&2
  exit 1
}

deadline_epoch=$(( $(date +%s) + max_seconds ))
last_nodes_json=''
for ((attempt = 1; attempt <= max_attempts; attempt++)); do
  # Cluster Autoscaler may register a replacement before its 2–3 minute
  # cold boot finishes. Never accept a stale or unhealthy node, but allow the
  # normal asynchronous replacement to converge within a finite window.
  if nodes_json=$("$kubectl_bin" --context "$kube_context" \
    --request-timeout=15s get nodes -o json 2>/dev/null); then
    last_nodes_json=$nodes_json
    if jq -e \
      --arg schematic "$expected_schematic" \
      --arg os_image "Talos ($expected_version)" \
      --argjson control_planes "$expected_control_planes" \
      --argjson workers "$expected_workers" \
      --argjson pools "$expected_migration_pools" '
        .items as $nodes |
        (.items | type) == "array" and
        ([.items[] | select(.metadata.name | startswith("prod-control-plane-"))] | length) == $control_planes and
        ([.items[] | select(.metadata.name | startswith("prod-worker-"))] | length) == $workers and
        all($pools[]; . as $pool |
          [$nodes[] | select(.metadata.name | startswith($pool.name + "-"))] as $members |
          ($members | length) >= $pool.min and ($members | length) <= $pool.max and
          all($members[];
            (.metadata.name | test("^" + $pool.name + "-[0-9a-f]{16}$")) and
            .metadata.labels["storage.platform.devantler.tech/migration-pool"] == $pool.role and
            .metadata.labels["node.kubernetes.io/instance-type"] == $pool.serverType
          )
        ) and
        all(.items[];
          .metadata.annotations["extensions.talos.dev/schematic"] == $schematic and
          .status.nodeInfo.osImage == $os_image and
          (.spec.unschedulable != true) and
          any(.status.conditions[]; .type == "Ready" and .status == "True")
        )
      ' <<<"$nodes_json" >/dev/null 2>&1; then
      printf 'PASS: all Talos nodes run %s with schematic %s and are Ready/schedulable\n' \
        "$expected_version" "$expected_schematic"
      exit 0
    fi
  fi

  if ((attempt == max_attempts || $(date +%s) >= deadline_epoch)); then
    break
  fi
  sleep "$interval_seconds"
done

printf 'Talos production node readback did not converge after %d attempt(s): expected version %s and schematic %s\n' \
  "$attempt" "$expected_version" "$expected_schematic" >&2
if [[ -n "$last_nodes_json" ]]; then
  jq -r '.items[]? | [.metadata.name, (.status.nodeInfo.osImage // "missing-version"),
    (.metadata.annotations["extensions.talos.dev/schematic"] // "missing-schematic"),
    (if .spec.unschedulable == true then "cordoned" else "schedulable" end),
    (if any(.status.conditions[]?; .type == "Ready" and .status == "True") then "Ready" else "NotReady" end)] | @tsv' \
    <<<"$last_nodes_json" >&2 || true
else
  printf 'Kubernetes nodes could not be read from context %s\n' "$kube_context" >&2
fi
exit 1
