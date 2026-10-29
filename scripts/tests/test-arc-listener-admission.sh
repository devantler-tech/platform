#!/usr/bin/env bash
# Distinct old/new objects exercise the real pinned Kyverno admission engine.
# No Kubernetes connection, credentials or live writes are used.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${1:-$root/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-openbao-listener-settings.yaml}"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
fail() {
  printf 'ARC listener admission: %s\n' "$*" >&2
  exit 1
}
[[ -f "$policy" ]] || fail 'missing listener policy'
yq -o=json '.' "$policy" | jq -e '
  .kind == "ValidatingPolicy" and .spec.failurePolicy == "Fail" and
  .spec.validationActions == ["Deny"] and .spec.evaluation.background.enabled == true and
  .spec.matchConstraints.resourceRules == [{apiGroups:[""],apiVersions:["*"],operations:["*"],resources:["configmaps"],resourceNames:["arc-openbao-listener"]}]
' >/dev/null || fail 'scope, background reporting or fail-closed disposition changed'
GITHUB_OUTPUT="$scratch/version" bash "$root/.github/scripts/kyverno-version.sh"
engine_version="$(cd "$root/scripts/tests/tenant-webapp-admission" && go list -m -f '{{.Version}}' github.com/kyverno/kyverno)"
[[ "$(<"$scratch/version")" == "release=$engine_version" ]] || fail 'engine does not match reviewed CLI pin'
# These literal source fixtures are independent of the replacement expression.
good="$(yq -o=json 'select(documentIndex == 0)' "$root/tests/restrict-arc-openbao-listener/resources.yaml")"
policy_name="$(yq -r '.metadata.name' "$policy")"
count=0
check() {
  local label="$1" object="$2" old="$3" operation="$4" expected="$5"
  jq -cn --arg label "$label" --arg policy "$policy_name" --argjson object "$object" --argjson old "$old" \
    --arg operation "$operation" --arg expected "$expected" \
    '{label:$label,policy:$policy,object:$object,oldObject:$old,actor:"synthetic",operation:$operation,expected:$expected}' >>"$scratch/controls.jsonl"
  count=$((count + 1))
}
check create-audited "$good" null CREATE pass
check update-audited "$good" "$good" UPDATE pass
check delete-audited null "$good" DELETE pass
# Every independent constraint must reject a newly introduced violation while
# preserving the documented allowance for an already violating UPDATE.
for change in \
  '.metadata.namespace = "elsewhere"' \
  '.data["listener.hcl"] += "synthetic change\n"' \
  '.data.extra = "synthetic"' \
  '.binaryData = {extra:"c3ludGhldGlj"}' \
  'del(.data)' \
  '.data = null' \
  '.data = {}'; do
  bad="$(jq "$change" <<<"$good")"
  check "create-rejected-$count" "$bad" null CREATE fail
  check "new-update-violation-$count" "$bad" "$good" UPDATE fail
  check "repair-update-$count" "$good" "$bad" UPDATE pass
  check "existing-update-violation-$count" "$bad" "$bad" UPDATE skip
  check "delete-rejected-$count" null "$bad" DELETE fail
done
for change in '.binaryData = null' '.binaryData = {}'; do
  equivalent="$(jq "$change" <<<"$good")"
  check "create-empty-binary-$count" "$equivalent" null CREATE pass
  check "update-empty-binary-$count" "$equivalent" "$good" UPDATE pass
  check "delete-empty-binary-$count" null "$equivalent" DELETE pass
done
# Different invalid contents still count as an existing violation. This cannot
# be represented by the CLI's UPDATE fixtures, which clone one object twice.
bad_old="$(jq '.data.extra="old"' <<<"$good")"
bad_new="$(jq '.data["listener.hcl"] += "new invalid content\n"' <<<"$good")"
check distinct-existing-violations "$bad_new" "$bad_old" UPDATE skip
(
  cd "$root/scripts/tests/tenant-webapp-admission"
  go test -mod=readonly -count=1 -timeout 60s ./...
  go build -mod=readonly -o "$scratch/admission" .
)
"$scratch/admission" "$policy" <"$scratch/controls.jsonl"
printf 'ARC listener admission: %s distinct-transition controls submitted.\n' "$count"
