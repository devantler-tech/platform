#!/usr/bin/env bash
# Derive the Deployment from the real graph; never maintain a second skeleton.
# This is offline policy evaluation, not KRO reconciliation or kubelet proof.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
policy="$root/k8s/bases/infrastructure/cluster-policies/best-practices/validate-pod-security.yaml"
GITHUB_OUTPUT="$scratch/version" bash "$root/.github/scripts/kyverno-version.sh"
expected="$(sed -n 's/^release=v//p' "$scratch/version")"
[ "$(kyverno version | sed -n 's/^Version: //p')" = "$expected" ] || {
  echo 'FAIL: the admission proof must use the reviewed Kyverno version' >&2
  exit 1
}

# Resolve both real API-defaulted instances through the pinned compiler/runtime.
# This proves dynamic expressions too; a hand-written empty-list substitution
# would conceal a changed projection or security expression from this policy.
PLATFORM_COMPILED_WEBAPP_DIR="$scratch/compiled" bash "$root/scripts/tests/test-webapp-compiler.sh" >"$scratch/compiler.log" 2>&1 || {
  cat "$scratch/compiler.log" >&2
  exit 1
}
for state in omitted enabled; do
cp "$scratch/compiled/$state.json" "$scratch/current.json"

jq 'del(.spec.template.spec.containers[].securityContext.runAsNonRoot)' "$scratch/current.json" >"$scratch/missing.json"
jq '.spec.template.spec.containers[].securityContext.runAsNonRoot = false' "$scratch/current.json" >"$scratch/false.json"
jq '.spec.template.spec.containers[].securityContext.runAsNonRoot = "true"' "$scratch/current.json" >"$scratch/string.json"

for control in current missing false string; do
  rc=0
  kyverno apply "$policy" -r "$scratch/$control.json" --set request.operation=CREATE \
    --policy-report --output-format=json --remove-color >"$scratch/$control-report.json" 2>"$scratch/$control.log" || rc=$?
  expected_fail=1
  [ "$control" != current ] || expected_fail=0
  # Policy-report mode returns its structured verdict. Some CLI versions return
  # zero for a report containing denials, so the exact report, never zero alone,
  # proves the outcome; operational errors and partial/no-evaluation reports fail.
  [ "$rc" -eq 0 ] || [ "$rc" -eq 1 ] || {
    cat "$scratch/$control.log" >&2
    exit "$rc"
  }
  [ "$control" != current ] || [ "$rc" -eq 0 ] || {
    echo 'FAIL: the current graph did not complete successfully' >&2
    exit 1
  }
  # The policy is a ValidatingPolicy: one result per resource, no rule names.
  # Its container-security expression shares one message with three sibling
  # conditions, so the denial is attributed by construction: the current graph
  # passes, and each control changes only runAsNonRoot from that graph.
  jq -es --argjson denied "$expected_fail" '
    length == 1 and (.[0] |
    .kind == "ClusterReport" and
    .summary == {pass:(1-$denied),fail:$denied,warn:0,error:0,skip:0} and
    (.results|length) == 1 and
    all(.results[]; .source == "KyvernoValidatingPolicy" and .policy == "validate-pod-security" and
      (.result == "pass" or .result == "fail") and
      .resources == [{kind:"Deployment",namespace:"runtime-fixture",name:"runtime-fixture",apiVersion:"apps/v1"}]) and
    all(.results[]|select(.result == "fail"); .message|contains("runAsNonRoot=true")))
  ' "$scratch/$control-report.json" >/dev/null || {
    echo "FAIL: actual WebApp container-security control $control did not evaluate as expected" >&2
    cat "$scratch/$control-report.json" >&2
    cat "$scratch/$control.log" >&2
    exit 1
  }
done
done
echo 'WebApp container security: actual omitted/enabled workloads accepted; missing/false/string declarations denied by the actual policy.'
