#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production controllers overlay'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the rendered Cilium policies'

rendered="$(kubectl kustomize "${controllers_dir}")" ||
  fail 'the production controllers overlay must render successfully'

# shellcheck disable=SC2016 # $policy is a yq variable, not a shell variable.
broad_authenticated_policies="$({
  printf '%s\n' "${rendered}" |
    yq ea -r '
      select(.kind == "CiliumClusterwideNetworkPolicy") as $policy |
      $policy.spec.ingress[]? |
      select(.authentication.mode == "required") |
      .fromEndpoints[]? |
      select(tag == "!!map" and length == 0) |
      $policy.metadata.name
    ' -
})" || fail 'the rendered Cilium policy inspection must succeed'

if [[ -n "${broad_authenticated_policies}" ]]; then
  fail "cluster-wide mutual-auth rules must not allow ingress from every endpoint: ${broad_authenticated_policies}"
fi

printf 'PASS: rendered cluster-wide mutual-auth rules preserve workload allow-lists\n'
