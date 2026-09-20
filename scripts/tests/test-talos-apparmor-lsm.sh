#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
config="$root_dir/talos/cluster/enable-apparmor.yaml"
cluster_config="$root_dir/ksail.prod.yaml"
ci_workflow="$root_dir/.github/workflows/ci.yaml"
expected_talos_version='v1.13.9'
expected='lsm=yama,selinux,loadpin,safesetid,integrity,bpf,apparmor,lockdown,landlock'

if ! grep -Fq "'scripts/tests/test-talos-apparmor-lsm.sh'" "$ci_workflow" ||
  ! grep -Fq 'bash scripts/tests/test-talos-apparmor-lsm.sh' "$ci_workflow"; then
  printf 'CI must detect and execute the Talos AppArmor LSM contract\n' >&2
  exit 1
fi

actual_talos_version=$(yq eval '.spec.cluster.talos.version' "$cluster_config")
[[ "$actual_talos_version" == "$expected_talos_version" ]] || {
  printf 'Talos changed from %s to %s; re-read that release kernel CONFIG_LSM and update this contract\n' \
    "$expected_talos_version" "$actual_talos_version" >&2
  exit 1
}

kernel_args=$(yq eval '.machine.install.extraKernelArgs[]' "$config")

printf '%s\n' "$kernel_args" | grep -Fxq "$expected" || {
  printf 'expected the Talos kernel LSM order %q; found: %s\n' "$expected" "$kernel_args" >&2
  exit 1
}

if printf '%s\n' "$kernel_args" | grep -q '^security='; then
  printf 'legacy security= selects one major LSM and must not accompany lsm=\n' >&2
  exit 1
fi

[[ $(printf '%s\n' "$kernel_args" | grep -c '^lsm=') -eq 1 ]] || {
  printf 'expected exactly one lsm= kernel argument\n' >&2
  exit 1
}

printf 'PASS: Talos enables AppArmor through the complete upstream LSM order\n'
