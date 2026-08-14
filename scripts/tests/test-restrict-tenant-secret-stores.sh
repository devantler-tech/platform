#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-tenant-secret-stores.yaml"
fixtures="${repo_root}/tests/restrict-tenant-secret-stores/resources.yaml"
values="${repo_root}/tests/restrict-tenant-secret-stores/values.yaml"
user_info="${repo_root}/tests/restrict-tenant-secret-stores/user-info.yaml"
output_file="$(mktemp)"
trap 'rm -f "${output_file}"' EXIT

# `kyverno test` treats a missing named rule as Excluded and can therefore pass
# vacuously. Exercise the policy directly as a second gate: the mixed fixture
# must contain two admitted resources and three denied resources.
if kyverno apply "${policy}" \
  --resource "${fixtures}" \
  --values-file "${values}" \
  --userinfo "${user_info}" \
  --remove-color >"${output_file}" 2>&1; then
  echo "::error::tenant SecretStore policy admitted every fixture; no deny rule executed"
  exit 1
fi

expected_summary="pass: 2, fail: 3, warn: 0, error: 0, skip: 0"
if ! grep -Fq "${expected_summary}" "${output_file}"; then
  echo "::error::tenant SecretStore policy returned an unexpected allow/deny verdict"
  sed -n '1,80p' "${output_file}"
  exit 1
fi

echo "Tenant SecretStore policy enforced the expected 2-pass/3-deny boundary."
