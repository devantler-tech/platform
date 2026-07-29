#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly vault_config_dir="${root_dir}/k8s/bases/infrastructure/vault-config"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

grep -Fq \
  "'scripts/tests/test-openbao-oidc-role.sh'" \
  "${ci_workflow}" ||
  fail 'the OpenBao OIDC role contract must trigger manifest validation'

grep -Fq \
  'run: bash scripts/tests/test-openbao-oidc-role.sh' \
  "${ci_workflow}" ||
  fail 'CI must execute the OpenBao OIDC role contract'

rendered="$(kubectl kustomize "${vault_config_dir}")" ||
  fail 'the OpenBao vault-config base must render'

role_payload="$(
  awk '
    /bao write auth\/oidc\/role\/admin - <<.*OIDC_ROLE_JSON/ {
      capture = 1
      next
    }
    capture && /^[[:space:]]*OIDC_ROLE_JSON[[:space:]]*$/ {
      complete = 1
      exit
    }
    capture {
      print
    }
    END {
      if (!capture || !complete) {
        exit 1
      }
    }
  ' <<<"${rendered}"
)" || fail 'the OpenBao admin role must be written as one JSON stdin payload'

jq -e '
  (.bound_claims | type) == "object" and
  .bound_claims.groups == ["devantler-tech:maintainers"] and
  .groups_claim == "groups" and
  .bound_audiences == ["public-client"] and
  .policies == ["vault-admin"]
' <<<"${role_payload}" >/dev/null ||
  fail 'the OpenBao admin role must bind the maintainer group as a JSON map'

printf 'PASS: OpenBao OIDC admin access is bound to the maintainer group with a typed JSON payload\n'
