#!/usr/bin/env bash
# A renamed/removed rule can look Excluded to kyverno test. This gate exercises
# the actual resource census and requires non-zero, exact Enforce results, and it
# pins the reviewed exclusion list so a widened exclusion cannot pass unnoticed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/audit-privileged-rbac.yaml"
grant="${repo_root}/k8s/bases/infrastructure/controllers/kyverno/cluster-role-read-rbac.yaml"
result="$(mktemp)"
trap 'rm -f "$result"' EXIT

# The mode lives on the rule. A spec-level validationFailureAction would be a second
# statement of it, and the `scored: "false"` annotation would report every rejection
# as a warn, so both must be absent.
yq -e '(.spec | has("validationFailureAction") | not) and .spec.background == true and
  (.spec.rules | length) == 1 and .spec.rules[0].validate.failureAction == "Enforce"' "$policy" >/dev/null
yq -e '.metadata.annotations | has("policies.kyverno.io/scored") | not' "$policy" >/dev/null
yq -o=json "$grant" | jq -e '
  .metadata.labels."rbac.kyverno.io/aggregate-to-reports-controller" == "true" and
  (.rules | length) == 1 and
  .rules[0].apiGroups == ["rbac.authorization.k8s.io"] and
  (.rules[0].resources | sort) == ["clusterroles", "roles"] and
  (.rules[0].verbs | sort) == ["get", "list", "watch"] and
  (.rules[0].resourceNames // [] | length) == 0
' >/dev/null

# The exclusions are exact identities: one kind per entry, names (plus namespaces for
# a Role) and nothing else. Any other selector — a label or namespace selector, or a
# userInfo block that exempts a requester rather than an object — would widen what
# Enforce lets through, so an entry carrying one fails here. The expanded
# kind|namespace|name set must equal the reviewed list exactly.
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
if ! yq -o=json '.spec.rules[0].exclude' "$policy" | jq -e --argjson expected "$expected" '
  (keys == ["any"]) and
  all(.any[];
    keys == ["resources"] and
    (.resources.kinds | length) == 1 and
    ((.resources | keys) == ["kinds", "names"] and
       .resources.kinds[0] == "rbac.authorization.k8s.io/v1/ClusterRole"
     or (.resources | keys) == ["kinds", "names", "namespaces"] and
       .resources.kinds[0] == "rbac.authorization.k8s.io/v1/Role")) and
  ([.any[].resources | .kinds[0] as $kind | (.namespaces // [""])[] as $ns
    | .names[] | "\($kind)|\($ns)|\(.)"] | sort) == ($expected | sort)
' >/dev/null; then
  echo 'RBAC exclusions differ from the reviewed exact-identity list' >&2
  exit 1
fi

# kyverno apply exits 1 when an Enforce rule fails, which the fixtures do on purpose;
# anything higher is a real error. The census is the assertion.
apply_status=0
kyverno apply "$policy" \
  --resource "${repo_root}/tests/audit-privileged-rbac/resources.yaml" \
  >"$result" 2>&1 || apply_status=$?
if [ "$apply_status" -gt 1 ] || ! grep -Fq 'pass: 15, fail: 24, warn: 0, error: 0, skip: 0' "$result"; then
  cat "$result"
  echo 'RBAC Enforce fixture census did not match the expected results' >&2
  exit 1
fi
echo 'RBAC Enforce: 15 ordinary or inert grants pass, 24 privileged grants fail, 2 reviewed exclusions skip; reporting access is read-only.'
