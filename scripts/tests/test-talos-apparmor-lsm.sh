#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
config=${1:-"$root_dir/talos/cluster/enable-apparmor.yaml"}
schematic=${2:-"$root_dir/talos/factory-schematic.yaml"}
cluster_config="$root_dir/ksail.prod.yaml"
ci_workflow="$root_dir/.github/workflows/ci.yaml"
expected_talos_version='v1.13.10'
expected_modules='apparmor,bpf,integrity,landlock,loadpin,lockdown,safesetid,selinux,yama'

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

check_args() {
  local file=$1 expression=$2 args modules
  args=$(yq eval -o=json "$expression" "$file")
  jq -e 'type == "array" and all(.[]; type == "string")' <<<"$args" >/dev/null || {
    printf '%s: kernel arguments must be a string array\n' "$file" >&2
    return 1
  }
  jq -e '[.[] | select(startswith("lsm="))] | length == 1' <<<"$args" >/dev/null || {
    printf '%s: expected exactly one lsm= kernel argument\n' "$file" >&2
    return 1
  }
  jq -e 'all(.[]; startswith("security=") | not)' <<<"$args" >/dev/null || {
    printf '%s: legacy security= must not accompany lsm=\n' "$file" >&2
    return 1
  }
  modules=$(jq '[.[] | select(startswith("lsm="))][0] | ltrimstr("lsm=") | split(",")' <<<"$args")
  jq -e --arg expected "$expected_modules" 'sort == ($expected | split(","))' <<<"$modules" >/dev/null || {
    printf '%s: preserve every Talos LSM exactly once\n' "$file" >&2
    return 1
  }
  # Linux 6.18 marks both major modules LSM_FLAG_EXCLUSIVE: the first wins.
  jq -e '[.[] | select(. == "selinux" or . == "apparmor")][0] == "apparmor"' <<<"$modules" >/dev/null || {
    printf '%s: SELinux precedes AppArmor and prevents AppArmor from activating\n' "$file" >&2
    return 1
  }
  # security_lsmprop_to_secctx(LSM_ID_UNDEF) returns the first hook result.
  # BPF ahead of AppArmor returns no context and triggers audit_log_subj_ctx.
  jq -e 'index("apparmor") < index("bpf")' <<<"$modules" >/dev/null || {
    printf '%s: AppArmor must precede BPF to provide the audit subject context\n' "$file" >&2
    return 1
  }
}

check_args "$config" '.machine.install.extraKernelArgs'
check_args "$schematic" '.customization.extraKernelArgs'

printf 'PASS: AppArmor wins major-LSM selection and precedes the BPF audit hook\n'
