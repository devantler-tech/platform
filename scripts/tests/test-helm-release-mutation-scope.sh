#!/usr/bin/env bash
# Pin the incoming-label boundary for the HelmRelease mutators (#5357).
# Kyverno 1.19.1's CEL CLI constructs UPDATE oldObject from the incoming resource;
# unlike the legacy engine it does not merge request.oldObject from values.yaml.
# Model a request already admitted by an old enabled label by opening ONLY the
# selector on a temporary copy. The real incoming-label condition must skip it;
# removing that condition must mutate it and fail the very same fixture.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
finished=0
# Preserve failure, including Bash 3.2's masked nounset abort before completion.
cleanup() {
  local rc=$?
  rm -rf "$work"
  if [[ "$finished" != 1 && $rc -eq 0 ]]; then
    echo 'helm-release-mutation-scope: aborted before finishing; reporting failure' >&2
    rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT

for suffix in enable-tests remediation-retries; do
  name="helm-release-$suffix"
  policy="$root/k8s/bases/infrastructure/cluster-policies/flux/$name.yaml"
  fixture="$root/tests/$name/label-removed"
  case_dir="$work/$name"
  mkdir -p "$case_dir"
  [[ -r "$policy" && -r "$fixture/kyverno-test.yaml" && -r "$fixture/resources.yaml" && -r "$fixture/values.yaml" ]] || {
    echo "UNKNOWN: incomplete $name scope fixture" >&2
    exit 2
  }
  # A renamed kind/selector requires an explicit update to this admission model.
  yq -e '.kind == "MutatingPolicy" and (.spec.matchConstraints.objectSelector.matchLabels | length == 1)' "$policy" >/dev/null
  cp "$fixture/resources.yaml" "$fixture/values.yaml" "$case_dir/"
  yq '.policies = ["policy.yaml"]' "$fixture/kyverno-test.yaml" >"$case_dir/kyverno-test.yaml"
  yq 'del(.spec.matchConstraints.objectSelector)' "$policy" >"$case_dir/policy.yaml"

  if ! bash "$root/scripts/validate-kyverno-fixture-evaluation.sh" "$case_dir" >"$work/positive.log" 2>&1; then
    tail -n 40 "$work/positive.log" >&2
    echo "FAIL: $name changed an incoming disabled or removed label" >&2
    exit 1
  fi

  # Same selected requests, same expected results, only the safety condition gone.
  yq -i 'del(.spec.matchConditions)' "$case_dir/policy.yaml"
  rc=0
  bash "$root/scripts/validate-kyverno-fixture-evaluation.sh" "$case_dir" >"$work/negative.log" 2>&1 || rc=$?
  if [[ $rc -ne 1 ]] || ! grep -qF 'Want skip, got pass' "$work/negative.log"; then
    tail -n 40 "$work/negative.log" >&2
    echo "FAIL: $name missing-condition control did not fail for an unexpected mutation (exit $rc)" >&2
    exit 1
  fi
done

echo 'PASS: modeled old-label admissions preserve incoming opt-out; both missing-condition controls are rejected'
finished=1
