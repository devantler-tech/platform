#!/usr/bin/env bash
# Derive the Deployment from the real graph; never maintain a second skeleton.
# This is offline policy evaluation, not KRO reconciliation or kubelet proof.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
graph="$root/k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml"
policy="$root/k8s/bases/infrastructure/cluster-policies/best-practices/validate-pod-security.yaml"
GITHUB_OUTPUT="$scratch/version" bash "$root/.github/scripts/kyverno-version.sh"
expected="$(sed -n 's/^release=v//p' "$scratch/version")"
[ "$(kyverno version | sed -n 's/^Version: //p')" = "$expected" ] || {
  echo 'FAIL: the admission proof must use the reviewed Kyverno version' >&2
  exit 1
}

# Only the graph's existing scalar inputs are realized. In particular, the
# securityContext objects below remain byte-for-value copies of the source.
yq -o=json '.spec.resources[] | select(.id == "deployment") | .template' "$graph" |
  jq '
    {"${schema.spec.name}":"webapp-admission-fixture",
     "${schema.spec.image}":"registry.k8s.io/pause:3.10.2",
     "${schema.spec.replicas}":2,"${schema.spec.port}":8080,
     "${schema.spec.probePath}":"/healthz",
     "${schema.spec.cpuRequest}":"10m","${schema.spec.memoryRequest}":"16Mi",
     "${schema.spec.cpuLimit}":"100m","${schema.spec.memoryLimit}":"64Mi"} as $values |
    walk(if type == "string" and contains("${") then
      . as $key | if $values|has($key) then $values[$key] else error("unknown graph expression") end
    else . end)' >"$scratch/current.json"

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
  # CLI 1.19.1 writes this exact API deprecation to stdout before its JSON.
  # Strip only that one known line; unexpected output still fails JSON parsing.
  warning="Warning: $policy: kyverno.io/v1 ClusterPolicy is deprecated and will be removed in a future release; migrate to ValidatingPolicy, MutatingPolicy, GeneratingPolicy or ImageValidatingPolicy (policies.kyverno.io), see https://kyverno.io/docs/guides/migration-to-cel/"
  if [ "$(head -n 1 "$scratch/$control-report.json")" = "$warning" ]; then
    tail -n +2 "$scratch/$control-report.json" >"$scratch/$control-verdict.json"
  else
    cp "$scratch/$control-report.json" "$scratch/$control-verdict.json"
  fi
  jq -es --argjson denied "$expected_fail" '
    length == 1 and (.[0] |
    .kind == "ClusterReport" and
    .summary == {pass:(3-$denied),fail:$denied,warn:0,error:0,skip:0} and
    (.results|length) == 3 and
    ([.results[].rule]|sort) == ["autogen-deny-container-seccomp-unconfined","autogen-require-seccomp-profile","autogen-validate-container-security"] and
    all(.results[]; .policy == "validate-pod-security" and
      (.result == "pass" or .result == "fail") and
      .resources == [{kind:"Deployment",namespace:"webapp-admission-fixture",name:"webapp-admission-fixture",apiVersion:"apps/v1"}]) and
    all(.results[]|select(.result == "fail"); .rule == "autogen-validate-container-security" and
      (.message|contains("/securityContext/runAsNonRoot/"))))
  ' "$scratch/$control-verdict.json" >/dev/null || {
    echo "FAIL: actual WebApp container-security control $control did not evaluate as expected" >&2
    cat "$scratch/$control-report.json" >&2
    cat "$scratch/$control.log" >&2
    exit 1
  }
done
echo 'WebApp container security: current graph accepted; missing/false/string declarations denied by the actual policy.'
