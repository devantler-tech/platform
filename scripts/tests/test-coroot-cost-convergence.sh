#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly production_infrastructure="${root_dir}/k8s/providers/hetzner/infrastructure"
readonly local_infrastructure="${root_dir}/k8s/providers/docker/infrastructure"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 ||
  fail 'kubectl is required to render the infrastructure overlays'
command -v yq >/dev/null 2>&1 ||
  fail 'yq v4 is required to inspect the rendered Coroot pricing contract'

grep -Fq \
  "'scripts/tests/test-coroot-cost-convergence.sh'" \
  "${ci_workflow}" ||
  fail 'the Coroot cost convergence contract must trigger manifest validation'

grep -Fq \
  'run: bash scripts/tests/test-coroot-cost-convergence.sh' \
  "${ci_workflow}" ||
  fail 'CI must execute the Coroot cost convergence contract'

production_rendered="$(kubectl kustomize "${production_infrastructure}")" ||
  fail 'the production infrastructure overlay must render'
local_rendered="$(kubectl kustomize "${local_infrastructure}")" ||
  fail 'the local infrastructure overlay must render'

if grep -Eqi 'opencost' <<<"${production_rendered}"; then
  fail 'production must not render an OpenCost workload, route, policy, or integration'
fi

if grep -Eqi 'opencost' <<<"${local_rendered}"; then
  fail 'local infrastructure must not render an OpenCost workload, route, policy, or integration'
fi

printf '%s\n' "${production_rendered}" |
  yq ea -e '
    select(
      .apiVersion == "batch/v1" and
      .kind == "CronJob" and
      .metadata.name == "coroot-custom-cloud-pricing" and
      .metadata.namespace == "observability"
    ) |
    [.spec.jobTemplate.spec.template.spec.containers[] |
      select(
        .name == "set-pricing" and
        ([.env[] | select(.name == "PER_CPU_CORE" and .value != "")] | length) == 1 and
        ([.env[] | select(.name == "PER_MEMORY_GB" and .value != "")] | length) == 1 and
        (.command | join("\n") | contains("/custom_cloud_pricing"))
      )] |
    length == 1
  ' - >/dev/null ||
  fail 'production must keep the Coroot custom cloud-pricing reconciler active'

printf 'PASS: Coroot is the only rendered cost-monitoring surface\n'
