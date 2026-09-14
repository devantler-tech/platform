#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly cd_workflow="${root_dir}/.github/workflows/cd.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect workflow behavior'

[[ "$(yq -r '.on.workflow_dispatch.inputs."retire-opencost".type // ""' "${cd_workflow}")" == 'boolean' ]] ||
  fail 'manual CD must expose retire-opencost as a Boolean input'
[[ "$(yq -r '.on.workflow_dispatch.inputs."retire-opencost".default | tostring' "${cd_workflow}")" == 'false' ]] ||
  fail 'manual CD must leave OpenCost retirement off by default'
# shellcheck disable=SC2016 # GitHub evaluates this expression, not Bash.
readonly expected_input_expression='${{ inputs.retire-opencost }}'
[[ "$(yq -r '.jobs."deploy-prod".steps[] | select(.uses == "./.github/actions/deploy-prod") | .with."retire-opencost" // ""' "${cd_workflow}")" == "${expected_input_expression}" ]] ||
  fail 'manual CD must forward only its explicit retirement input to the protected deploy action'

[[ "$(yq -r '.inputs."retire-opencost".default | tostring' "${deploy_action}")" == 'false' ]] ||
  fail 'the shared production deploy must leave OpenCost retirement off by default'
[[ "$(yq -r '.inputs."retire-opencost".required | tostring' "${deploy_action}")" == 'false' ]] ||
  fail 'the retirement input must remain optional for merge-group and heal deployments'

retire_condition="$(yq -r '.runs.steps[] | select(.id == "retire_opencost") | .if // ""' "${deploy_action}")"
[[ "${retire_condition}" == "inputs.retire-opencost == 'true'" ]] ||
  fail 'the destructive retirement step must run only when its composite input is exactly true'
retire_run="$(yq -r '.runs.steps[] | select(.id == "retire_opencost") | .run // ""' "${deploy_action}")"
[[ "${retire_run}" == './scripts/retire-opencost.sh --execute' ]] ||
  fail 'the opted-in step must invoke the hard-coded retirement script with its execution guard'

wait_index="$(yq -r '.runs.steps | to_entries[] | select(.value.id == "wait_flux_revision") | .key' "${deploy_action}")"
retire_index="$(yq -r '.runs.steps | to_entries[] | select(.value.id == "retire_opencost") | .key' "${deploy_action}")"
cluster_update_index="$(yq -r '.runs.steps | to_entries[] | select(.value.name == "🔄 Update cluster (Talos machine config)") | .key' "${deploy_action}")"
[[ "${wait_index}" =~ ^[0-9]+$ && "${retire_index}" =~ ^[0-9]+$ && "${cluster_update_index}" =~ ^[0-9]+$ ]] ||
  fail 'the retirement ordering steps must all exist'
[[ "${wait_index}" -lt "${cluster_update_index}" && "${cluster_update_index}" -lt "${retire_index}" ]] ||
  fail 'irreversible retirement must run only after exact Flux convergence and the ordinary deployment completes'

if yq -e '.jobs[]?.steps[]? | select(.uses == "./.github/actions/deploy-prod") | .with | has("retire-opencost")' "${ci_workflow}" >/dev/null 2>&1; then
  fail 'merge-group and heal deployments must not opt into the irreversible retirement'
fi

printf 'PASS: OpenCost retirement is manual-main-only, default-off, and ordered after the ordinary deployment\n'
