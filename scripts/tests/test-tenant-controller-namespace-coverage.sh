#!/usr/bin/env bash
# Replay the complete tenant admission fixtures under both real ownership labels.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
failures=0
while IFS=$'\t' read -r family passed denied skipped; do
  for owner in ksail kro; do
    OWNER="$owner" yq '(.namespaceSelector[] | select(.labels."app.kubernetes.io/managed-by" == "ksail") | .labels."app.kubernetes.io/managed-by") = strenv(OWNER)' \
      "$root/tests/$family/values.yaml" >"$work/values.yaml"
    args=(--resource "$root/tests/$family/resources.yaml" --values-file "$work/values.yaml" --remove-color)
    if [[ -f "$root/tests/$family/user-info.yaml" ]]; then
      args+=(--userinfo "$root/tests/$family/user-info.yaml")
    fi
    status=0
    kyverno apply "$root/k8s/bases/infrastructure/cluster-policies/best-practices/$family.yaml" "${args[@]}" >"$work/output" 2>&1 || status=$?
    expected="pass: $passed, fail: $denied, warn: 0, error: 0, skip: $skipped"
    if [[ "$status" != 1 ]] || ! grep -Fq "$expected" "$work/output"; then
      printf 'FAIL: %s owner=%s expected %s (exit 1), got exit %s\n' "$family" "$owner" "$expected" "$status" >&2
      tail -n 3 "$work/output" >&2
      failures=$((failures + 1))
    fi
  done
done <<'CASES'
restrict-tenant-secret-stores	5	7	0
restrict-tenant-issuer-refs	4	3	0
restrict-tenant-route-hostnames	6	18	0
restrict-tenant-network-policies	19	80	1
restrict-tenant-image-repositories	7	11	0
CASES
[[ "$failures" == 0 ]] || exit 1
echo 'PASS: all five tenant admission families evaluate their full controls under ksail and kro ownership'
