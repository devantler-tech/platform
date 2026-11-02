#!/usr/bin/env bash
# A renamed/removed rule can look Excluded to kyverno test. This gate exercises
# the actual resource census and requires non-zero, exact Enforce results, and it
# pins the policy's whole enforcement shape, so any edit that exempts more objects
# or enforces less fails here instead of passing unnoticed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/audit-privileged-rbac.yaml"
grant="${repo_root}/k8s/bases/infrastructure/controllers/kyverno/cluster-role-read-rbac.yaml"
result="$(mktemp)"
trap 'rm -f "$result"' EXIT

yq -o=json "$grant" | jq -e '
  .metadata.labels."rbac.kyverno.io/aggregate-to-reports-controller" == "true" and
  (.rules | length) == 1 and
  .rules[0].apiGroups == ["rbac.authorization.k8s.io"] and
  (.rules[0].resources | sort) == ["clusterroles", "roles"] and
  (.rules[0].verbs | sort) == ["get", "list", "watch"] and
  (.rules[0].resourceNames // [] | length) == 0
' >/dev/null

# EXACT SHAPE, not a blocklist. Many single-field edits weaken an Enforce policy while
# leaving its expression untouched: validationActions back to Audit, admission turned
# off, match operations limited to CREATE, a matchConditions entry keyed on a label, or
# a `scored: "false"` annotation that reports every rejection as a warn. Pinning the
# complete key set of each level rejects all of them, and any field Kyverno adds later,
# until this test is changed in review.
#
# The exclusions are one list of exact kind|namespace|name identities (the namespace is
# empty for a ClusterRole), so a Role exclusion never covers the same name in another
# namespace or as a ClusterRole. The identity expression is pinned verbatim, and the
# list must equal the reviewed list exactly. Folding the list into the validation rather
# than a matchConditions entry makes the background scan record a current `pass` for an
# accepted role instead of leaving its last failure in the report (platform#3900).
expected='[
  "ClusterRole||admin",
  "ClusterRole||cluster-admin",
  "ClusterRole||crossplane",
  "ClusterRole||crossplane-rbac-manager",
  "ClusterRole||crossplane:system:aggregate-to-crossplane",
  "ClusterRole||edit",
  "ClusterRole||kro-tenant-rgd",
  "ClusterRole||kro:controller",
  "ClusterRole||ksail-operator",
  "ClusterRole||longhorn-role",
  "ClusterRole||system:aggregate-to-edit",
  "ClusterRole||system:controller:clusterrole-aggregation-controller",
  "Role|longhorn-system|longhorn",
  "Role|velero|velero-server"
]'
if ! yq -o=json '.' "$policy" | jq -e --argjson expected "$expected" '
  def literal: type == "string" and length > 0 and (test("[*?]") | not);
  ([.spec.variables[0].expression | scan("\\x27([^\\x27]*)\\x27") | .[0]]) as $listed |
  .apiVersion == "policies.kyverno.io/v1" and .kind == "ValidatingPolicy" and
  .metadata.name == "audit-privileged-rbac" and
  (.metadata | keys) == ["annotations", "name"] and
  (.metadata.annotations | has("policies.kyverno.io/scored") | not) and
  (.spec | keys) == ["evaluation", "failurePolicy", "matchConstraints", "validationActions", "validations", "variables"] and
  .spec.failurePolicy == "Fail" and
  .spec.validationActions == ["Deny"] and
  .spec.evaluation == {"background": {"enabled": true}} and
  .spec.matchConstraints == {"resourceRules": [{
    "apiGroups": ["rbac.authorization.k8s.io"],
    "apiVersions": ["v1"],
    "operations": ["CREATE", "UPDATE"],
    "resources": ["roles", "clusterroles"]}]} and
  ([.spec.variables[].name]) == ["reviewedExclusions", "reviewed"] and
  all(.spec.variables[]; keys == ["expression", "name"]) and
  .spec.variables[1].expression ==
    "(object.kind + '"'|'"' + object.metadata.?namespace.orValue('"''"') + '"'|'"' + object.metadata.name) in variables.reviewedExclusions" and
  all($listed[]; literal) and ($listed | sort) == ($expected | sort) and
  (.spec.validations | length) == 1 and
  (.spec.validations[0] | keys) == ["expression", "message"] and
  # Kubernetes CEL types Role.rules and ClusterRole.rules as lists. has() covers
  # absence and the type() guard preserves YAML-null fixtures; comparing the typed
  # list value directly with null is rejected by the live Kyverno policy compiler.
  (.spec.validations[0].expression |
    startswith("variables.reviewed || !has(object.rules) || type(object.rules) == null_type || object.rules.all(rule,"))
' >/dev/null; then
  echo 'RBAC policy differs from the reviewed Enforce shape or exact-identity exclusion list' >&2
  exit 1
fi

# kyverno apply exits 1 when an Enforce rule fails, which the fixtures do on purpose;
# anything higher is a real error. `pass: 17, fail: 24` counts both exclusion fixtures
# among the passes; the control below proves they pass only because they are listed.
apply_status=0
kyverno apply "$policy" \
  --resource "${repo_root}/tests/audit-privileged-rbac/resources.yaml" \
  >"$result" 2>&1 || apply_status=$?
if [ "$apply_status" -gt 1 ] || ! grep -Fq 'pass: 17, fail: 24, warn: 0, error: 0, skip: 0' "$result"; then
  cat "$result"
  echo 'RBAC Enforce fixture census did not match the expected results' >&2
  exit 1
fi

# The census counts results but not whose they are, and kyverno test pairs rows by
# name alone, so it cannot tell velero/velero-server from its sample/ lookalike. The
# policy report names every resource: the two reviewed fixtures must pass and each
# same-name lookalike must still fail, with every result from the background scan.
identities='[.results[] | {
    id: "\(.resources[0].kind)|\(.resources[0].namespace // "")|\(.resources[0].name)",
    result, process: .properties.process
  }]'
report_status=0
kyverno apply "$policy" \
  --resource "${repo_root}/tests/audit-privileged-rbac/resources.yaml" \
  --policy-report --output-format json >"$result" 2>&1 || report_status=$?
if [ "$report_status" -gt 1 ] || ! awk '/^\{/ { found = 1 } found' "$result" | jq -e "
  ${identities} as \$all |
  all(\$all[]; .process == \"background scan\") and
  ([\$all[] | select(.id == (
      \"ClusterRole||crossplane-rbac-manager\",
      \"Role|velero|velero-server\")) | .result] == [\"pass\", \"pass\"]) and
  ([\$all[] | select(.id == (
      \"ClusterRole||crossplane-rbac-manager-lookalike\",
      \"ClusterRole||velero-server\",
      \"Role|sample|velero-server\")) | .result] == [\"fail\", \"fail\", \"fail\"])
" >/dev/null; then
  cat "$result"
  echo 'RBAC policy report did not pass exactly the reviewed exclusions' >&2
  exit 1
fi

# Control: a pass on those two fixtures means nothing unless the list is what produced
# it. With the list emptied, the same two must fail and nothing else may change.
# kyverno apply loads a policy file only by its .yaml extension.
control_dir="$(mktemp -d)"
trap 'rm -rf "$result" "$control_dir"' EXIT
empty_list_policy="${control_dir}/policy.yaml"
yq '(.spec.variables[] | select(.name == "reviewedExclusions") | .expression) = "dyn([])"' \
  "$policy" >"$empty_list_policy"
control_status=0
kyverno apply "$empty_list_policy" \
  --resource "${repo_root}/tests/audit-privileged-rbac/resources.yaml" \
  --policy-report --output-format json >"$result" 2>&1 || control_status=$?
if [ "$control_status" -gt 1 ] || ! awk '/^\{/ { found = 1 } found' "$result" | jq -e "
  ${identities} as \$all |
  ([\$all[] | select(.result == \"fail\")] | length) == 26 and
  ([\$all[] | select(.result == \"pass\")] | length) == 15 and
  ([\$all[] | select(.id == (
      \"ClusterRole||crossplane-rbac-manager\",
      \"Role|velero|velero-server\")) | .result] == [\"fail\", \"fail\"])
" >/dev/null; then
  cat "$result"
  echo 'RBAC exclusion control: the reviewed fixtures did not fail with the list emptied' >&2
  exit 1
fi
echo 'RBAC Enforce: 15 ordinary or inert grants pass, 24 privileged grants fail, 2 reviewed exclusions pass only because they are listed; reporting access is read-only.'
