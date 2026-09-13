#!/usr/bin/env bash
set -euo pipefail

# Positive gate for the hcloud block-storage guards (#3287).
#
# An hcloud volume attaches to one node and is not force-detached when that
# node departs, so every workload that mounts one must require a static
# baseline worker. This renders the prod (hetzner) overlays and asserts, for
# each such workload, that EVERY required nodeSelectorTerm carries
# `platform.devantler.tech/baseline-worker In ["true"]`. Terms are ORed, so a
# single term without it would fail open.
#
# Coroot is checked generically: every object in the Coroot CR spec whose
# `storage.className` is hcloud must carry the rule in its sibling `affinity`,
# so a component that gains hcloud storage later fails here until it is
# guarded. The base's clickhouse/keeper podAntiAffinity must survive the merge.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
infra="${repo_root}/k8s/providers/hetzner/infrastructure"
controllers="${infra}/controllers"
label='platform.devantler.tech/baseline-worker'
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

kubectl kustomize "${infra}" >"${work}/infra.yaml"
kubectl kustomize "${controllers}" >"${work}/controllers.yaml"
[ -s "${work}/infra.yaml" ] || { echo "::error::infrastructure overlay rendered nothing"; exit 1; }
[ -s "${work}/controllers.yaml" ] || { echo "::error::controllers overlay rendered nothing"; exit 1; }

fail=0
checked=0

# check_affinity <what> <file holding one affinity document as YAML>
# Passes only when the required terms are non-empty and every term requires the
# label with exactly the value "true".
check_affinity() {
  local what="$1" file="$2" terms bad
  terms="$(yq '.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // [] | length' "${file}")"
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

# OpenBao: `server.affinity` is a templated string — parse it as YAML. The
# template expressions sit in quoted/label positions, so the YAML still parses.
yq ea 'select(.kind == "HelmRelease" and .metadata.name == "openbao") | .spec.values.server.affinity // ""' \
  "${work}/controllers.yaml" | sed -e 's/{{[^}]*}}/tmpl/g' >"${work}/openbao.yaml"
if [ ! -s "${work}/openbao.yaml" ]; then
  echo "::error::openbao HelmRelease server.affinity did not render"
  fail=1
else
  check_affinity "openbao server" "${work}/openbao.yaml"
  anti="$(yq '.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution // [] | length' "${work}/openbao.yaml")"
  [ "${anti}" -ge 1 ] || { echo "::error::openbao server lost its required podAntiAffinity"; fail=1; }
fi

# Vault snapshot workloads: both mount the hcloud-backed vault-snapshots PVC.
extract "${work}/infra.yaml" '.kind == "CronJob" and .metadata.name == "vault-snapshot"' \
  '.spec.jobTemplate.spec.template.spec.affinity' "${work}/cron.yaml"
check_affinity "vault-snapshot CronJob" "${work}/cron.yaml"
extract "${work}/infra.yaml" '.kind == "Job" and .metadata.name == "vault-snapshot-init"' \
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

if [ "${fail}" -ne 0 ]; then
  exit 1
fi
echo "hcloud-baseline-worker-affinity: ${checked} hcloud consumers require ${label}=true in every term"
