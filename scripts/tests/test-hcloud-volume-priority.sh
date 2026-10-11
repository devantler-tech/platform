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

# Admission only handles new Pods. Existing databases need their owning Cluster
# declaration to request the tier so CNPG can roll immutable Pod priority.
databases=''
tenant_patches=''
for layer in apps infrastructure; do
  rendered="$(kubectl kustomize "${repo_root}/k8s/providers/hetzner/${layer}")"
  databases+="$(yq ea -o=json -I=0 'select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster")' <<<"${rendered}")"$'\n'
  if [[ "${layer}" == apps ]]; then
    tenant_patches="$(yq ea -e 'select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and .metadata.namespace == "wedding-app" and .metadata.name == "wedding-app") | .spec.patches' <<<"${rendered}")"
  fi
done
# The parent render contains only the tenant's Flux wiring, not its database.
# Exercise the delivered patch list as Flux does against an unprioritised tenant
# Cluster; checking the patch source text alone would miss an unwired override.
fixture_dir="$(mktemp -d)"
trap 'rm -r -- "${fixture_dir}"' EXIT
cp "${script_dir}/fixtures/wedding-database-priority/cluster.yaml" "${fixture_dir}/cluster.yaml"
printf '%s\n' "${tenant_patches}" >"${fixture_dir}/patches.yaml"
PRIORITY_PATCHES="${fixture_dir}/patches.yaml" yq -n '.apiVersion = "kustomize.config.k8s.io/v1beta1" | .kind = "Kustomization" |
  .resources = ["cluster.yaml"] | .patches = load(strenv(PRIORITY_PATCHES))' \
  >"${fixture_dir}/kustomization.yaml"
tenant_render="$(kubectl kustomize "${fixture_dir}")"
tenant_database="$(yq ea -o=json -I=0 'select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster" and .metadata.name == "wedding-db" and .metadata.namespace == "wedding-app")' <<<"${tenant_render}")"
if [[ "$(jq -s 'length' <<<"${tenant_database}")" -ne 1 ]]; then
  echo 'FAIL: tenant delivery must cover exactly one Wedding database' >&2
  exit 1
fi
databases+="${tenant_database}"$'\n'
database_count="$(jq -s 'length' <<<"${databases}")"
if [[ "${database_count}" -le 1 ]]; then
  echo 'FAIL: no platform CNPG Cluster was checked alongside the tenant database' >&2
  exit 1
fi
missing="$(jq -sr --arg n "${class_name}" '.[] | select(.spec.priorityClassName != $n) | .metadata.name' <<<"${databases}")"
if [[ -n "${missing}" ]]; then
  echo "FAIL: production databases do not declare ${class_name}: ${missing}" >&2
  exit 1
fi
# CNPG 1.30.1 does not include priorityClassName in its Pod-spec comparison.
# The common supported restart request must reach every existing Cluster.
requests="$(jq -s '[.[].metadata.annotations["kubectl.kubernetes.io/restartedAt"]] | unique' <<<"${databases}")"
if ! jq -e 'length == 1 and (.[0] | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))' <<<"${requests}" >/dev/null; then
  echo 'FAIL: production databases lack a common controlled rolling-restart request' >&2
  exit 1
fi
echo "OK: ${database_count} production databases declare the storage priority tier"
