#!/usr/bin/env bash
set -euo pipefail

# Positive gate for the baseline-worker label policies (#3287).
#
# `kyverno test` reports a rule that matches nothing as "skip", so the fixture
# alone passes vacuously if the match ever stops firing — including the
# autoscaler-node control, which would then prove nothing. This gate reads the
# mutated resources and asserts both states, and pins the properties that keep
# the policies safe:
#   * failurePolicy Ignore on both policies, so a Kyverno outage never rejects
#     Node registration;
#   * the retrofit policy's match and target name the SAME nodes as the
#     admission policy, so the two halves cannot drift apart;
#   * both halves set the same label key and value.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
policies="${repo_root}/k8s/providers/hetzner/infrastructure/cluster-policies"
admission="${policies}/label-baseline-workers.yaml"
retrofit="${policies}/label-existing-baseline-workers.yaml"
fixtures="${repo_root}/tests/label-baseline-workers/resources.yaml"
label='platform.devantler.tech/baseline-worker'
out_dir="$(mktemp -d)"
trap 'rm -rf "${out_dir}"' EXIT

fail=0
expect() {
  local what="$1" want="$2" got="$3"
  if [ "${got}" != "${want}" ]; then
    echo "::error::${what}: expected '${want}', got '${got}'"
    fail=1
  fi
}

if ! kyverno apply "${admission}" \
  --resource "${fixtures}" \
  --output "${out_dir}" \
  --remove-color >"${out_dir}/apply.log" 2>&1; then
  echo "::error::kyverno apply failed for label-baseline-workers"
  sed -n '1,60p' "${out_dir}/apply.log"
  exit 1
fi

# Every Node document kyverno wrote, keyed by node name → label value. `kyverno
# apply --output` writes every resource, mutated or not, so each control must be
# PRESENT with the label unset; a node missing from the output entirely means
# the fixture did not render and fails as `<not-rendered>`.
mutated="${out_dir}/mutated.tsv"
found=0
for f in "${out_dir}"/*.yaml; do
  [ -f "${f}" ] || continue
  found=1
  yq -N ea "select(.kind == \"Node\") | .metadata.name + \"\t\" + (.metadata.labels.\"${label}\" // \"<unset>\")" "${f}" >>"${mutated}"
done
if [ "${found}" -ne 1 ] || [ ! -s "${mutated}" ]; then
  echo "::error::kyverno apply emitted no mutated Node; the rule did not fire"
  sed -n '1,60p' "${out_dir}/apply.log"
  exit 1
fi

value_for() {
  awk -F'\t' -v n="$1" '$1 == n { print $2; found = 1 } END { if (!found) print "<not-rendered>" }' "${mutated}"
}

# ON state — the assertions the vacuity trap hides.
expect "prod-worker-1 label" true "$(value_for prod-worker-1)"
expect "prod-worker-2 (no other labels) label" true "$(value_for prod-worker-2)"
# OFF state — an autoscaler node identical except for its name, and a
# control-plane node, are rendered but never labelled.
expect "autoscaler node label" '<unset>' "$(value_for autoscale-cx43-59ee3c84869749a0)"
expect "control-plane node label" '<unset>' "$(value_for prod-control-plane-2)"

# Safety properties of both policies.
expect "admission failurePolicy" Ignore "$(yq '.spec.failurePolicy' "${admission}")"
expect "retrofit failurePolicy" Ignore "$(yq '.spec.failurePolicy' "${retrofit}")"
expect "retrofit mutateExistingOnPolicyUpdate" true "$(yq '.spec.mutateExistingOnPolicyUpdate' "${retrofit}")"

admission_names="$(yq -o=json -I=0 '.spec.rules[0].match.any[0].resources.names' "${admission}")"
expect "retrofit match names" "${admission_names}" \
  "$(yq -o=json -I=0 '.spec.rules[0].match.any[0].resources.names' "${retrofit}")"
expect "retrofit target name" "$(yq '.spec.rules[0].match.any[0].resources.names[0]' "${admission}")" \
  "$(yq '.spec.rules[0].mutate.targets[0].name' "${retrofit}")"
expect "retrofit target kind" "v1/Node" \
  "$(yq '.spec.rules[0].mutate.targets[0].apiVersion + "/" + .spec.rules[0].mutate.targets[0].kind' "${retrofit}")"
expect "retrofit label value" true \
  "$(yq ".spec.rules[0].mutate.patchStrategicMerge.metadata.labels.\"${label}\"" "${retrofit}")"
expect "admission label value" true \
  "$(yq ".spec.rules[0].mutate.patchStrategicMerge.metadata.labels.\"${label}\"" "${admission}")"

if [ "${fail}" -ne 0 ]; then
  exit 1
fi
echo "label-baseline-workers: baseline workers labelled, controls untouched, safety properties hold"
