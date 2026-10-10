#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null || fail 'kubectl is required'
command -v yq >/dev/null || fail 'yq is required'

for path in k8s/bases/apps/backstage k8s/providers/hetzner/apps/backstage docs/backstage.md; do
  [[ ! -e "${root_dir}/${path}" ]] || fail "retired scaffold remains at ${path}"
done

# Examine every independently reconciled production layer, including embedded
# proxy, authentication and monitoring configuration. A removed app directory
# alone cannot prove that shared layers stopped granting it access.
for layer in bootstrap infrastructure/controllers infrastructure apps; do
  rendered="$(kubectl kustomize "${root_dir}/k8s/providers/hetzner/${layer}")" ||
    fail "production ${layer} must render"
  documents="$(printf '%s\n' "${rendered}" | yq ea -o=json -I=0 '.' -)" ||
    fail "production ${layer} must decode"
  if grep -qi backstage <<< "${documents}"; then
    fail "production ${layer} still deploys or references the retired portal"
  fi
done

printf 'PASS: every production layer is free of the retired portal and its access wiring\n'
