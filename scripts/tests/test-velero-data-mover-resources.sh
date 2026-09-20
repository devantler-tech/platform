#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly node_agent_config="${root_dir}/k8s/bases/infrastructure/controllers/velero/config-map.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the Velero node-agent configuration'

if ! grep -Fq "'scripts/tests/test-velero-data-mover-resources.sh'" "${ci_workflow}" ||
  ! grep -Fq 'bash scripts/tests/test-velero-data-mover-resources.sh' "${ci_workflow}"; then
  fail 'CI must detect and execute the Velero data-mover resource contract'
fi

memory_limit="$(yq -er '.data["node-agent-config.json"] | from_json | .podResources.memoryLimit' "${node_agent_config}")" ||
  fail 'the Velero data-mover memory limit is missing'
readonly memory_limit

[[ "${memory_limit}" == '1Gi' ]] ||
  fail "Velero data-mover pods need a 1Gi memory limit; found ${memory_limit}"

printf 'Velero data-mover resource contract is valid.\n'
