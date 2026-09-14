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
# The exclusions are exact identities: one kind per entry, at least one literal name
# (an empty list means "any name" to Kyverno), exactly one namespace for a Role, and no
# `*` or `?`, which Kyverno treats as wildcards. The expanded kind|namespace|name set
# must equal the reviewed list exactly.
expected='[
  "rbac.authorization.k8s.io/v1/ClusterRole||admin",
  "rbac.authorization.k8s.io/v1/ClusterRole||cluster-admin",
  "rbac.authorization.k8s.io/v1/ClusterRole||crossplane",
  "rbac.authorization.k8s.io/v1/ClusterRole||crossplane-rbac-manager",
  "rbac.authorization.k8s.io/v1/ClusterRole||crossplane:system:aggregate-to-crossplane",
  "rbac.authorization.k8s.io/v1/ClusterRole||edit",
  "rbac.authorization.k8s.io/v1/ClusterRole||kro-tenant-rgd",
  "rbac.authorization.k8s.io/v1/ClusterRole||kro:controller",
  "rbac.authorization.k8s.io/v1/ClusterRole||ksail-operator",
  "rbac.authorization.k8s.io/v1/ClusterRole||longhorn-role",
  "rbac.authorization.k8s.io/v1/ClusterRole||system:aggregate-to-edit",
  "rbac.authorization.k8s.io/v1/ClusterRole||system:controller:clusterrole-aggregation-controller",
  "rbac.authorization.k8s.io/v1/Role|longhorn-system|longhorn",
  "rbac.authorization.k8s.io/v1/Role|velero|velero-server"
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
  (.spec.rules[0] | keys) == ["exclude", "match", "name", "validate"] and
  .spec.rules[0].name == "privileged-rules" and
  .spec.rules[0].match == {"any": [{"resources": {"kinds": [
    "rbac.authorization.k8s.io/v1/Role",
    "rbac.authorization.k8s.io/v1/ClusterRole"]}}]} and
  (.spec.rules[0].validate | keys) == ["cel", "failureAction"] and
  .spec.rules[0].validate.failureAction == "Enforce" and
  (.spec.rules[0].validate.cel | keys) == ["expressions"] and
  (.spec.rules[0].validate.cel.expressions | length) == 1 and
  (.spec.rules[0].validate.cel.expressions[0] | keys) == ["expression", "message"] and
  (.spec.rules[0].exclude | keys) == ["any"] and
  all(.spec.rules[0].exclude.any[];
    keys == ["resources"] and
    (.resources.kinds | length) == 1 and
    (.resources.names | type) == "array" and (.resources.names | length) >= 1 and
    all(.resources.names[]; literal) and
    (((.resources | keys) == ["kinds", "names"] and
        .resources.kinds[0] == "rbac.authorization.k8s.io/v1/ClusterRole")
     or ((.resources | keys) == ["kinds", "names", "namespaces"] and
        .resources.kinds[0] == "rbac.authorization.k8s.io/v1/Role" and
        (.resources.namespaces | length) == 1 and
        (.resources.namespaces[0] | literal)))) and
  ([.spec.rules[0].exclude.any[].resources | .kinds[0] as $kind
    | (.namespaces // [""])[] as $ns | .names[] | "\($kind)|\($ns)|\(.)"] | sort)
    == ($expected | sort)
' >/dev/null; then
  echo 'RBAC policy differs from the reviewed Enforce shape or exact-identity exclusion list' >&2
  exit 1
fi

# kyverno apply exits 1 when an Enforce rule fails, which the fixtures do on purpose;
# anything higher is a real error. The census is the assertion: excluded objects are
# not evaluated, so they appear in no count. `fail: 24` proves both exclusion fixtures
# really were excluded (it would be 26 otherwise) while their lookalikes were not.
apply_status=0
kyverno apply "$policy" \
  --resource "${repo_root}/tests/audit-privileged-rbac/resources.yaml" \
  >"$result" 2>&1 || apply_status=$?
if [ "$apply_status" -gt 1 ] || ! grep -Fq 'pass: 15, fail: 24, warn: 0, error: 0, skip: 0' "$result"; then
  cat "$result"
  echo 'RBAC Enforce fixture census did not match the expected results' >&2
  exit 1
fi
echo 'RBAC Enforce: 15 ordinary or inert grants pass, 24 privileged grants fail, 2 reviewed exclusions are not evaluated; reporting access is read-only.'
