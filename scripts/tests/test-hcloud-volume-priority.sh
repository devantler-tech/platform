#!/usr/bin/env bash
# The storage priority policies write both the class name and its
# integer value onto a Pod. The Priority admission plugin refuses a Pod whose
# spec.priority differs from its class's value, so if these two files drift
# apart every Pod that mounts an hcloud volume is rejected at creation.
# This check fails CI before that can ship (platform#4031).
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
class_file="${repo_root}/k8s/bases/bootstrap/priority-classes/hcloud-volume-workload.yaml"
policy_dir="${repo_root}/k8s/providers/hetzner/infrastructure/cluster-policies"

class_name="$(yq -e 'select(.kind == "PriorityClass") | .metadata.name' "${class_file}")"
class_value="$(yq -e 'select(.kind == "PriorityClass") | .value' "${class_file}")"

# Every patch, whether directly under mutate or inside a foreach. The database
# policy must use the same class and value: a lower tier remains a victim.
patches="$(yq ea -o=json -I=0 '[.. | select(has("patchStrategicMerge")) | .patchStrategicMerge.spec]' \
  "${policy_dir}/prioritise-hcloud-volume-pods.yaml" \
  "${policy_dir}/prioritise-database-pods.yaml")"
count="$(jq 'length' <<<"${patches}")"
if [[ "${count}" -ne 3 ]]; then
  echo "FAIL: expected three storage priority patches, found ${count}" >&2
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
