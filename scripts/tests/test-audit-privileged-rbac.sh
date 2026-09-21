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
# leaving its expression untouched: spec.admission false, failureActionOverrides back to
# Audit, match operations limited to CREATE, preconditions keyed on a label, a
# webhookConfiguration matchCondition, or a `scored: "false"` annotation that reports
# every rejection as a warn. Pinning the complete key set of each level rejects all of
# them, and any field Kyverno adds later, until this test is changed in review.
#
# The exclusions are one false precondition keyed on the exact kind|namespace|name of
# the object (the namespace is empty for a ClusterRole), so a Role exclusion never
# covers the same name in another namespace or as a ClusterRole. The key is pinned
# verbatim, the values carry no `*` or `?` (Kyverno treats both as wildcards), and the
# value set must equal the reviewed list exactly. A precondition rather than an
# `exclude` makes the background scan record a current `skip` for an accepted role
# instead of leaving its last warning in the report (platform#3900).
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
  .apiVersion == "kyverno.io/v1" and .kind == "ClusterPolicy" and
  .metadata.name == "audit-privileged-rbac" and
  (.metadata | keys) == ["annotations", "name"] and
  (.metadata.annotations | has("policies.kyverno.io/scored") | not) and
  (.spec | keys) == ["background", "rules"] and
  .spec.background == true and
  (.spec.rules | length) == 1 and
  (.spec.rules[0] | keys) == ["match", "name", "preconditions", "validate"] and
  .spec.rules[0].name == "privileged-rules" and
  .spec.rules[0].match == {"any": [{"resources": {"kinds": [
    "rbac.authorization.k8s.io/v1/Role",
    "rbac.authorization.k8s.io/v1/ClusterRole"]}}]} and
  (.spec.rules[0].validate | keys) == ["cel", "failureAction"] and
  .spec.rules[0].validate.failureAction == "Enforce" and
  (.spec.rules[0].validate.cel | keys) == ["expressions"] and
  (.spec.rules[0].validate.cel.expressions | length) == 1 and
  (.spec.rules[0].validate.cel.expressions[0] | keys) == ["expression", "message"] and
  # Kubernetes CEL types Role.rules and ClusterRole.rules as lists. has() covers
  # absence and the type() guard preserves YAML-null fixtures; comparing the typed
  # list value directly with null is rejected by the live Kyverno policy compiler.
  (.spec.rules[0].validate.cel.expressions[0].expression |
    startswith("!has(object.rules) || type(object.rules) == null_type || object.rules.all(rule,")) and
  (.spec.rules[0].preconditions | keys) == ["all"] and
  (.spec.rules[0].preconditions.all | length) == 1 and
  (.spec.rules[0].preconditions.all[0] | keys) == ["key", "operator", "value"] and
  .spec.rules[0].preconditions.all[0].key ==
    "{{ request.object.kind || request.oldObject.kind || '"''"' }}|{{ request.object.metadata.namespace || request.oldObject.metadata.namespace || '"''"' }}|{{ request.object.metadata.name || request.oldObject.metadata.name || '"''"' }}" and
  .spec.rules[0].preconditions.all[0].operator == "AnyNotIn" and
  all(.spec.rules[0].preconditions.all[0].value[]; literal) and
  (.spec.rules[0].preconditions.all[0].value | sort) == ($expected | sort)
' >/dev/null; then
  echo 'RBAC policy differs from the reviewed Enforce shape or exact-identity exclusion list' >&2
  exit 1
fi

# kyverno apply exits 1 when an Enforce rule fails, which the fixtures do on purpose;
# anything higher is a real error. `fail: 24, skip: 2` proves both exclusion fixtures
# were skipped (fail would be 26 otherwise) while their lookalikes were not.
apply_status=0
kyverno apply "$policy" \
  --resource "${repo_root}/tests/audit-privileged-rbac/resources.yaml" \
  >"$result" 2>&1 || apply_status=$?
if [ "$apply_status" -gt 1 ] || ! grep -Fq 'pass: 15, fail: 24, warn: 0, error: 0, skip: 2' "$result"; then
  cat "$result"
  echo 'RBAC Enforce fixture census did not match the expected results' >&2
  exit 1
fi

# The census counts results but not whose they are, and kyverno test pairs rows by
# name alone, so it cannot tell velero/velero-server from its sample/ lookalike. The
# policy report names every resource: exactly the two reviewed fixtures must be a
# background-scan `skip`, and each same-name lookalike must still fail.
report_status=0
kyverno apply "$policy" \
  --resource "${repo_root}/tests/audit-privileged-rbac/resources.yaml" \
  --policy-report --output-format json >"$result" 2>&1 || report_status=$?
if [ "$report_status" -gt 1 ] || ! awk '/^\{/ { found = 1 } found' "$result" | jq -e '
  [.results[] | {
    id: "\(.resources[0].kind)|\(.resources[0].namespace // "")|\(.resources[0].name)",
    result, process: .properties.process
  }] as $all |
  ([$all[] | select(.result == "skip") | .id] | sort) ==
    ["ClusterRole||crossplane-rbac-manager", "Role|velero|velero-server"] and
  all($all[] | select(.result == "skip"); .process == "background scan") and
  ([$all[] | select(.id == (
      "ClusterRole||crossplane-rbac-manager-lookalike",
      "ClusterRole||velero-server",
      "Role|sample|velero-server")) | .result] == ["fail", "fail", "fail"])
' >/dev/null; then
  cat "$result"
  echo 'RBAC policy report did not skip exactly the reviewed exclusions' >&2
  exit 1
fi
echo 'RBAC Enforce: 15 ordinary or inert grants pass, 24 privileged grants fail, 2 reviewed exclusions are recorded as a current skip; reporting access is read-only.'
