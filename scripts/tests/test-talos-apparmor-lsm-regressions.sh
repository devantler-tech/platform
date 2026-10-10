#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
guard="$root_dir/scripts/tests/test-talos-apparmor-lsm.sh"
config="$root_dir/talos/cluster/enable-apparmor.yaml"
schematic="$root_dir/talos/factory-schematic.yaml"
fixture_dir=$(mktemp -d)
fixture="$fixture_dir/args.json"
output="$fixture_dir/output"
trap 'rm -f "$fixture" "$output"; rmdir "$fixture_dir"' EXIT

bash "$guard" "$config" "$schematic"

# Exercise the actual guard through both public inputs, not a duplicate predicate.
# Valid JSON fixtures keep parser failures from masquerading as a rejected LSM order.
for declaration in config schematic; do
  if [[ "$declaration" == config ]]; then
    args=$(yq eval -o=json '.machine.install.extraKernelArgs' "$config")
  else
    args=$(yq eval -o=json '.customization.extraKernelArgs' "$schematic")
  fi

  for mutation in selinux-first bpf-first missing-module disabled missing-enable conflicting-enable; do
    case "$mutation" in
      selinux-first)
        filter='map(if startswith("lsm=") then sub("apparmor,selinux"; "selinux,apparmor") else . end)'
        diagnostic='SELinux precedes AppArmor'
        ;;
      bpf-first)
        filter='map(if startswith("lsm=") then sub(",bpf,"; ",") | sub("apparmor,"; "bpf,apparmor,") else . end)'
        diagnostic='AppArmor must precede BPF'
        ;;
      missing-module)
        filter='map(if startswith("lsm=") then sub(",landlock"; "") else . end)'
        diagnostic='preserve every Talos LSM exactly once'
        ;;
      disabled)
        filter='map(if . == "apparmor=1" then "apparmor=0" else . end)'
        diagnostic='expected exactly one apparmor= kernel argument, apparmor=1'
        ;;
      missing-enable)
        filter='map(select(startswith("apparmor=") | not))'
        diagnostic='expected exactly one apparmor= kernel argument, apparmor=1'
        ;;
      conflicting-enable)
        filter='. + ["apparmor=0"]'
        diagnostic='expected exactly one apparmor= kernel argument, apparmor=1'
        ;;
    esac
    mutated=$(jq "$filter" <<<"$args")
    [[ "$mutated" != "$args" ]] || {
      printf '%s/%s: counterfactual did not change the arguments\n' "$declaration" "$mutation" >&2
      exit 1
    }
    jq -n --argjson args "$mutated" \
      '{machine: {install: {extraKernelArgs: $args}}, customization: {extraKernelArgs: $args}}' >"$fixture"
    inputs=("$config" "$schematic")
    if [[ "$declaration" == config ]]; then
      inputs[0]="$fixture"
    else
      inputs[1]="$fixture"
    fi
    if bash "$guard" "${inputs[@]}" >"$output" 2>&1; then
      printf '%s/%s: guard accepted an unsafe configuration\n' "$declaration" "$mutation" >&2
      exit 1
    fi
    grep -Fq "$diagnostic" "$output" || {
      printf '%s/%s: failed for the wrong reason\n' "$declaration" "$mutation" >&2
      exit 1
    }
    printf 'PASS: %s rejects %s\n' "$declaration" "$mutation"
  done
done
