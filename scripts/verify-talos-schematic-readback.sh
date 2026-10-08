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

[[ "$expected_schematic" =~ ^[0-9a-f]{64}$ &&
   "$expected_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ &&
   "$expected_control_planes" =~ ^[1-9][0-9]*$ &&
   "$expected_workers" =~ ^[1-9][0-9]*$ ]] || {
  printf 'invalid desired Talos version, schematic, or static-node count\n' >&2
  exit 1
}

# A deploy that changes the image, upgrades one node and then fails leaves that node on the new
# image. This branch cannot roll a node to another image at the same Talos version, so without
# an exception every later deploy fails here and the repair itself cannot land (#4665). The
# reviewed record below may therefore accept ONE static node on ONE named image until a date.
# It is read whole before any node is: a record that is not exactly that is refused, never
# guessed at. An accepted node is reported as a warning and never as the plain pass, and every
# other requirement (Talos version, Ready, schedulable, node counts) still binds it. Adding and
# removing a row: docs/operations/boot-image-drift.md.
accepted_file=${TALOS_READBACK_ACCEPTED_DRIFT_FILE:-$root_dir/scripts/talos-boot-image-drift-accepted.tsv}
today=${TALOS_READBACK_TODAY:-$(date -u +%F)}
date_re='^20[0-9]{2}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])$'
[[ "$today" =~ $date_re ]] || {
  printf 'invalid Talos readback date\n' >&2
  exit 1
}
invalid_record() {
  printf 'invalid accepted boot-image drift record: %s\n' "$1" >&2
  exit 1
}
accepted_node=''
accepted_schematic=''
accepted_issue=''
accepted_expires=''
recorded_node=''
if [[ -e "$accepted_file" ]]; then
  [[ -f "$accepted_file" && -r "$accepted_file" ]] || invalid_record 'the file cannot be read'
  line_number=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line_number=$((line_number + 1))
    if ((line_number == 1)); then
      [[ "$line" == $'node\tschematic\tissue\texpires' ]] || invalid_record 'header'
      continue
    fi
    ((line_number == 2)) || invalid_record 'more than one row'
    IFS=$'\t' read -r -a fields <<<"$line"
    # A trailing empty field is dropped by read, so count the separators as well.
    separators=${line//[^$'\t']/}
    [[ "${#fields[@]}" == 4 && "${#separators}" == 3 ]] || invalid_record 'fields'
    [[ "${fields[0]}" =~ ^prod-(control-plane|worker)-[0-9]+$ ]] || invalid_record 'node'
    [[ "${fields[1]}" =~ ^[0-9a-f]{64}$ && "${fields[1]}" != "$expected_schematic" ]] ||
      invalid_record 'schematic'
    [[ "${fields[2]}" =~ ^devantler-tech/platform#[1-9][0-9]*$ ]] || invalid_record 'issue'
    [[ "${fields[3]}" =~ $date_re ]] || invalid_record 'expires'
    recorded_node=${fields[0]}
    # ISO dates order as text; the record holds through its last day.
    if [[ "${fields[3]}" < "$today" ]]; then
      printf 'the accepted boot-image drift for %s expired on %s; the readback is strict again\n' \
        "${fields[0]}" "${fields[3]}" >&2
      continue
    fi
    accepted_node=${fields[0]}
    accepted_schematic=${fields[1]}
    accepted_issue=${fields[2]}
    accepted_expires=${fields[3]}
  done <"$accepted_file"
  ((line_number >= 1)) || invalid_record 'header'
fi

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
      --arg accepted_node "$accepted_node" \
      --arg accepted_schematic "$accepted_schematic" \
      --argjson control_planes "$expected_control_planes" \
      --argjson workers "$expected_workers" '
        (.items | type) == "array" and
        ([.items[] | select(.metadata.name | startswith("prod-control-plane-"))] | length) == $control_planes and
        ([.items[] | select(.metadata.name | startswith("prod-worker-"))] | length) == $workers and
        all(.items[];
          (.metadata.annotations["extensions.talos.dev/schematic"] == $schematic or
           ($accepted_node != "" and .metadata.name == $accepted_node and
            .metadata.annotations["extensions.talos.dev/schematic"] == $accepted_schematic)) and
          .status.nodeInfo.osImage == $os_image and
          (.spec.unschedulable != true) and
          any(.status.conditions[]; .type == "Ready" and .status == "True")
        )
      ' <<<"$nodes_json" >/dev/null 2>&1; then
      # Which of the two ways did the recorded node pass? Count the nodes off the expected image:
      # the test above admits at most the recorded one.
      drifted=$(jq -r --arg schematic "$expected_schematic" \
        '[.items[] | select(.metadata.annotations["extensions.talos.dev/schematic"] != $schematic)] | length' \
        <<<"$nodes_json")
      total=$(jq -r '.items | length' <<<"$nodes_json")
      if [[ "$drifted" == 0 ]]; then
        if [[ -n "$recorded_node" ]]; then
          printf '::notice::the accepted boot-image drift for %s is no longer needed; remove its row\n' \
            "$recorded_node"
        fi
        printf 'PASS: all Talos nodes run %s with schematic %s and are Ready/schedulable\n' \
          "$expected_version" "$expected_schematic"
        exit 0
      fi
      [[ "$drifted" == 1 && -n "$accepted_node" ]] || {
        printf 'Talos readback counted %s node(s) off the expected image after admitting them\n' \
          "$drifted" >&2
        exit 1
      }
      printf '::warning::%s runs the recorded boot image %s instead of %s (%s, accepted until %s)\n' \
        "$accepted_node" "$accepted_schematic" "$expected_schematic" "$accepted_issue" "$accepted_expires"
      printf 'PASS-WITH-ACCEPTED-DRIFT: 1 of %s Talos nodes runs a recorded boot image; the others run %s with schematic %s, and all are Ready/schedulable\n' \
        "$total" "$expected_version" "$expected_schematic"
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
