#!/usr/bin/env bash
# The prioritise-hcloud-volume-pods policy writes both the class name and its
# integer value onto a Pod. The Priority admission plugin refuses a Pod whose
# spec.priority differs from its class's value, so if these two files drift
# apart every Pod that mounts an hcloud volume is rejected at creation.
# This check fails CI before that can ship (platform#4031).
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
class_file="${repo_root}/k8s/bases/bootstrap/priority-classes/hcloud-volume-workload.yaml"
policy_file="${repo_root}/k8s/providers/hetzner/infrastructure/cluster-policies/prioritise-hcloud-volume-pods.yaml"

class_name="$(yq -e 'select(.kind == "PriorityClass") | .metadata.name' "${class_file}")"
class_value="$(yq -e 'select(.kind == "PriorityClass") | .value' "${class_file}")"

# Every patch in the policy, whether directly under mutate or inside a foreach.
patches="$(yq -o=json -I=0 '[.. | select(has("patchStrategicMerge")) | .patchStrategicMerge.spec]' "${policy_file}")"
count="$(jq 'length' <<<"${patches}")"
if [[ "${count}" -lt 2 ]]; then
  echo "FAIL: expected a patch per rule (claims and ephemeral volumes), found ${count}" >&2
  exit 1
fi

bad="$(jq -r --arg n "${class_name}" --argjson v "${class_value}" \
  '.[] | select(.priorityClassName != $n or .priority != $v) | tojson' <<<"${patches}")"
if [[ -n "${bad}" ]]; then
  echo "FAIL: a patch does not match PriorityClass ${class_name}=${class_value}:" >&2
  echo "${bad}" >&2
  exit 1
fi

echo "OK: ${count} patches set ${class_name} with priority ${class_value}"
