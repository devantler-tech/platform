#!/usr/bin/env bash
set -euo pipefail

# Positive gate for the hcloud block-storage guards (#3287).
#
# An hcloud volume attaches to one node and is not force-detached when that
# node departs, so a workload that mounts one must require a static baseline
# worker. This renders the prod (hetzner) `infrastructure` overlay and asserts,
# for each guarded workload, that EVERY required nodeSelectorTerm carries
# `platform.devantler.tech/baseline-worker In ["true"]`. Terms are ORed, so a
# single term without it would fail open.
#
# The label rule covers the `infrastructure` layer, because the
# label-baseline-workers policies that stamp the label are applied in that same
# layer. OpenBao deploys earlier, in `infrastructure-controllers`, so requiring
# the label there would deadlock a rebuilt cluster whose nodes are not labelled
# yet. OpenBao is checked separately below: it must require the static workers
# by hostname.
#
# Coroot is checked generically: every object in the Coroot CR spec whose
# `storage.className` is hcloud must carry the rule in its sibling `affinity`,
# so a component that gains hcloud storage later fails here until it is
# guarded. The base's clickhouse/keeper podAntiAffinity must survive the merge.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
infra="${repo_root}/k8s/providers/hetzner/infrastructure"
label='platform.devantler.tech/baseline-worker'
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

kubectl kustomize "${infra}" >"${work}/infra.yaml"
[ -s "${work}/infra.yaml" ] || { echo "::error::infrastructure overlay rendered nothing"; exit 1; }

fail=0
checked=0

# check_affinity <what> <file holding one affinity document as YAML>
# Passes only when the required terms are non-empty and every term requires the
# label with exactly the value "true".
check_affinity() {
  local what="$1" file="$2" terms bad docs
  # Exactly one document: a selector matching two resources would make yq print
  # one number per document, and a non-integer must never pass the checks below.
  docs="$(yq ea '[.] | length' "${file}")"
  if [ "${docs}" != "1" ]; then
    echo "::error::${what}: expected exactly one rendered document, got '${docs}'"
    fail=1
    return
  fi
  terms="$(yq '.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // [] | length' "${file}")"
  case "${terms}" in
    '' | *[!0-9]*)
      echo "::error::${what}: unreadable nodeSelectorTerms count '${terms}'"
      fail=1
      return
      ;;
  esac
  if [ "${terms}" -eq 0 ]; then
    echo "::error::${what}: no required nodeSelectorTerms"
    fail=1
    return
  fi
  bad="$(yq "[.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[]
          | select(([.matchExpressions // [] | .[]
                     | select(.key == \"${label}\" and .operator == \"In\"
                              and (.values | length) == 1 and .values[0] == \"true\")]
                    | length) == 0)] | length" "${file}")"
  if [ "${bad}" -ne 0 ]; then
    echo "::error::${what}: ${bad} of ${terms} required nodeSelectorTerm(s) do not require ${label}=true"
    fail=1
    return
  fi
  checked=$((checked + 1))
}

# extract <rendered file> <yq selector for the document> <yq path to affinity> <out>
extract() {
  local src="$1" doc="$2" path="$3" out="$4"
  yq ea "select(${doc}) | ${path} // {}" "${src}" >"${out}"
}

# Vault snapshot workloads: both mount the hcloud-backed vault-snapshots PVC.
extract "${work}/infra.yaml" '.kind == "CronJob" and .metadata.name == "vault-snapshot" and .metadata.namespace == "openbao"' \
  '.spec.jobTemplate.spec.template.spec.affinity' "${work}/cron.yaml"
check_affinity "vault-snapshot CronJob" "${work}/cron.yaml"
extract "${work}/infra.yaml" '.kind == "Job" and .metadata.name == "vault-snapshot-init" and .metadata.namespace == "openbao"' \
  '.spec.template.spec.affinity' "${work}/init.yaml"
check_affinity "vault-snapshot-init Job" "${work}/init.yaml"

# Coroot: every spec object with hcloud storage, found by walking the CR.
yq ea 'select(.kind == "Coroot" and .metadata.name == "coroot") | .spec' "${work}/infra.yaml" >"${work}/coroot.yaml"
[ -s "${work}/coroot.yaml" ] || { echo "::error::Coroot CR did not render"; fail=1; }
coroot_paths=()
# The root spec itself has no path segment; yq prints an empty line for it.
while IFS= read -r p; do
  coroot_paths+=("${p}")
done < <(yq '[.. | select(tag == "!!map" and .storage.className == "hcloud") | path | join(".")] | .[]' "${work}/coroot.yaml")
if [ "${#coroot_paths[@]}" -lt 4 ]; then
  echo "::error::expected >=4 Coroot components with hcloud storage (server, prometheus, clickhouse, keeper), found ${#coroot_paths[@]}"
  fail=1
fi
for p in "${coroot_paths[@]}"; do
  if [ -z "${p}" ]; then
    name="server"
    sel='.affinity // {}'
  else
    name="${p}"
    sel=".${p}.affinity // {}"
  fi
  yq "${sel}" "${work}/coroot.yaml" >"${work}/coroot-affinity.yaml"
  check_affinity "coroot ${name}" "${work}/coroot-affinity.yaml"
done
for c in clickhouse clickhouse.keeper; do
  anti="$(yq ".${c}.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution // [] | length" "${work}/coroot.yaml")"
  [ "${anti}" -ge 1 ] || { echo "::error::coroot ${c} lost the base's required podAntiAffinity"; fail=1; }
done

# OpenBao: requires the static workers BY HOSTNAME, not by the label. It deploys
# in `infrastructure-controllers`, before the label policies apply, so a label
# requirement would deadlock a rebuilt cluster. The kubelet sets
# kubernetes.io/hostname at registration, so this rule holds from the first
# reconcile. The single required term must name exactly
# ${cluster_name}-worker-1..N, where N is the static worker count in
# ksail.prod.yaml. The patch is shared by every Hetzner cluster, so the name
# prefix is the Flux substitution variable, never a literal; prod's value of it
# must equal ksail.prod.yaml's metadata.name, which is what KSail names nodes by.
controllers="${repo_root}/k8s/providers/hetzner/infrastructure/controllers"
kubectl kustomize "${controllers}" >"${work}/controllers.yaml"
[ -s "${work}/controllers.yaml" ] || { echo "::error::infrastructure-controllers overlay rendered nothing"; exit 1; }
workers="$(yq '.spec.cluster.workers' "${repo_root}/ksail.prod.yaml")"
case "${workers}" in
  '' | 0 | *[!0-9]*)
    echo "::error::unreadable static worker count '${workers}' in ksail.prod.yaml"
    exit 1
    ;;
esac
prod_name="$(yq '.metadata.name' "${repo_root}/ksail.prod.yaml")"
prod_cluster_name="$(yq 'select(.kind == "ConfigMap" and .metadata.name == "variables-cluster") | .data.cluster_name' \
  "${repo_root}/k8s/clusters/prod/bootstrap/config-map.yaml")"
if [ -z "${prod_name}" ] || [ "${prod_name}" = "null" ] || [ "${prod_cluster_name}" != "${prod_name}" ]; then
  echo "::error::prod variables-cluster cluster_name '${prod_cluster_name}' must equal ksail.prod.yaml metadata.name '${prod_name}'"
  fail=1
fi
# The chart's server.affinity is a templated string; replace each Helm
# expression with a placeholder so the rest parses as YAML.
yq ea 'select(.kind == "HelmRelease" and .metadata.name == "openbao" and .metadata.namespace == "openbao") | .spec.values.server.affinity' \
  "${work}/controllers.yaml" | sed -E 's/\{\{[^}]*\}\}/helm-template/g' >"${work}/openbao-affinity.yaml"
if ! openbao_terms="$(yq -o=json -I=0 '.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // []' "${work}/openbao-affinity.yaml")"; then
  echo "::error::OpenBao server.affinity did not parse"
  fail=1
else
  want="$(jq -cn --argjson n "${workers}" \
    '[{"matchExpressions": [{"key": "kubernetes.io/hostname", "operator": "In", "values": [range(1; $n + 1) | "${cluster_name}-worker-\(.)"]}]}]')"
  got="$(jq -c . <<<"${openbao_terms}")"
  if [ "${got}" != "${want}" ]; then
    echo "::error::OpenBao must require exactly one nodeSelectorTerm ${want}, got ${got}"
    fail=1
  else
    checked=$((checked + 1))
  fi
  anti="$(yq '[.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution // [] | .[] | select(.topologyKey == "kubernetes.io/hostname")] | length' "${work}/openbao-affinity.yaml")"
  [ "${anti}" -ge 1 ] || { echo "::error::OpenBao lost the chart's required hostname podAntiAffinity"; fail=1; }
fi

if [ "${fail}" -ne 0 ]; then
  exit 1
fi
echo "hcloud-baseline-worker-affinity: ${checked} hcloud consumers require ${label}=true in every term"
