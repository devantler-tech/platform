#!/usr/bin/env bash
# A VPA must follow the actual workload incarnation, and invalid UIDs must fail
# evaluation rather than silently generate an unusable owner reference.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
for kind in deployment statefulset daemonset; do
  fixture="$root/tests/auto-vpa-generating-policy-$kind"
  case_dir="$work/$kind"
  mkdir -p "$case_dir"
  cp "$fixture/"*.yaml "$case_dir/"
  POLICY="$root/k8s/bases/infrastructure/cluster-policies/best-practices/auto-vpa-$kind.yaml" \
    yq -i '.policies = [strenv(POLICY)]' "$case_dir/kyverno-test.yaml"
  kyverno test "$case_dir" --remove-color >"$case_dir/baseline.log" 2>&1
  replacement=10000000-0000-0000-0000-000000000001
  UID_VALUE="$replacement" yq -i '.metadata.uid = strenv(UID_VALUE)' "$case_dir/resources.yaml"
  UID_VALUE="$replacement" yq -i '.metadata.ownerReferences[0].uid = strenv(UID_VALUE)' "$case_dir/generated-vpa.yaml"
  kyverno test "$case_dir" --remove-color >"$case_dir/replacement.log" 2>&1
  # The old owner must be rejected for the replacement with the same name.
  cp "$fixture/generated-vpa.yaml" "$case_dir/generated-vpa.yaml"
  if kyverno test "$case_dir" --remove-color >"$case_dir/old-owner.log" 2>&1; then
    echo "FAIL: $kind accepted the previous incarnation"; exit 1
  fi
  for invalid in missing empty numeric; do
    cp "$fixture/resources.yaml" "$case_dir/resources.yaml"
    case "$invalid" in
      missing) yq -i 'del(.metadata.uid)' "$case_dir/resources.yaml" ;;
      empty) yq -i '.metadata.uid = ""' "$case_dir/resources.yaml" ;;
      numeric) yq -i '.metadata.uid = 42' "$case_dir/resources.yaml" ;;
    esac
    if kyverno test "$case_dir" --remove-color >"$case_dir/$invalid.log" 2>&1; then
      echo "FAIL: $kind accepted a $invalid UID"; exit 1
    fi
    if ! grep -Eqi 'error|failed|not found' "$case_dir/$invalid.log"; then
      cat "$case_dir/$invalid.log"; echo "FAIL: $kind/$invalid failed without evaluation evidence"; exit 1
    fi
  done
  echo "ok: $kind binds the replacement UID and refuses missing, empty and numeric UIDs"
done
