#!/usr/bin/env bash
# Distinct old/new objects exercise the real pinned Kyverno admission engine for
# restrict-github-team-external-identity. The CLI clones an UPDATE's object, so
# every transition lives here: each tests/restrict-github-team-external-identity
# directory with an expected.yaml states literal outcomes, and its values.yaml
# states the previous object. No Kubernetes connection or credentials are used.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${1:-$root/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-github-team-external-identity.yaml}"
fixtures="$root/tests/restrict-github-team-external-identity"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
fail() {
  printf 'GitHub team identity admission: %s\n' "$*" >&2
  exit 1
}
[[ -f "$policy" ]] || fail 'missing policy'
yq -o=json '.' "$policy" | jq -e '
  .kind == "ValidatingPolicy" and .spec.failurePolicy == "Fail" and
  .spec.validationActions == ["Deny"] and .spec.evaluation.background.enabled == false and
  .spec.matchConstraints.resourceRules == [{
    apiGroups: ["team.github.m.upbound.io"], apiVersions: ["*"], operations: ["CREATE", "UPDATE"],
    resources: ["teams", "teamrepositories", "teammemberships"]}]
' >/dev/null || fail 'scope, operations or fail-closed disposition changed'
GITHUB_OUTPUT="$scratch/version" bash "$root/.github/scripts/kyverno-version.sh"
engine_version="$(cd "$root/scripts/tests/tenant-webapp-admission" && go list -m -f '{{.Version}}' github.com/kyverno/kyverno)"
[[ "$(<"$scratch/version")" == "release=$engine_version" ]] || fail 'engine does not match reviewed CLI pin'
policy_name="$(yq -r '.metadata.name' "$policy")"
applier='system:serviceaccount:github-config:github-config'
controls="$scratch/controls.jsonl"
: >"$controls"

# A resource named in expected.yaml and missing from resources.yaml is a failed
# read here, never a row that silently drops out.
for expected in "$fixtures"/*/expected.yaml; do
  case_dir="$(dirname "$expected")"
  case_name="$(basename "$case_dir")"
  yq -o=json -I=0 'select(.kind != null)' "$case_dir/resources.yaml" | jq -s '.' >"$scratch/resources.json"
  yq -o=json '.policies[0].resources // []' "$case_dir/values.yaml" >"$scratch/values.json"
  yq -o=json '.results' "$expected" >"$scratch/results.json"
  actor="$(yq -r '.userInfo.username' "$case_dir/user-info.yaml")"
  jq -c --arg case "$case_name" --arg policy "$policy_name" --arg actor "$actor" \
    --slurpfile resources "$scratch/resources.json" --slurpfile values "$scratch/values.json" '
    .[] | .result as $expected | .kind as $kind | .resources[] | (split("/")[1]) as $name
    | ([$resources[0][] | select(.kind == $kind and .metadata.name == $name)]) as $found
    | if ($found | length) != 1 then error("\($case): \($name) is not exactly one fixture resource") else . end
    | $found[0] as $object
    | ([$values[0][] | select(.name == $name) | .values["request.oldObject"]] | first // {}) as $changes
    | {label: "\($case)/\($name)", policy: $policy, object: $object, oldObject: ($object * $changes),
       actor: $actor, operation: "UPDATE", expected: $expected}' \
    "$scratch/results.json" >>"$controls" || fail "could not build $case_name"
done
count="$(grep -c . "$controls")"
[[ "$count" -eq 34 ]] || fail "$count transitions built, expected the 34 fixture rows"

# Independent boundary controls, built from a literal foreign identity.
team() {
  jq -cn --arg namespace "$1" --arg identity "$2" '{apiVersion: "team.github.m.upbound.io/v1beta1", kind: "Team",
    metadata: {name: "admins", namespace: $namespace, annotations: {"crossplane.io/external-name": $identity}},
    spec: {forProvider: {privacy: "closed"}}}'
}
control() {
  jq -cn --arg label "$1" --arg policy "$policy_name" --arg actor "$2" --argjson object "$3" --arg expected "$4" \
    '{label: $label, policy: $policy, object: $object, oldObject: null, actor: $actor, operation: "CREATE", expected: $expected}' \
    | jq -c 'del(.oldObject)' >>"$controls"
}
foreign_team="$(team github-config 99999999)"
provider_prefix='system:serviceaccount:crossplane-system:provider-upjet-github-'
control foreign-from-applier "$applier" "$foreign_team" fail
control approved-from-applier "$applier" "$(team github-config 18573307)" pass
control other-namespace "$applier" "$(team elsewhere 99999999)" skip
control provider-account "${provider_prefix}f5d83a3e04ea" "$foreign_team" skip
# Only the exact namespace and the whole prefix earn the exemption.
control provider-name-in-other-namespace 'system:serviceaccount:github-config:provider-upjet-github-f5d83a3e04ea' "$foreign_team" fail
control other-provider-package 'system:serviceaccount:crossplane-system:provider-upjet-gitlab-f5d83a3e04ea' "$foreign_team" fail
control prefix-as-a-user-name 'provider-upjet-github-f5d83a3e04ea' "$foreign_team" fail
count="$(grep -c . "$controls")"

(
  cd "$root/scripts/tests/tenant-webapp-admission"
  go test -mod=readonly -count=1 -timeout 60s ./...
  go build -mod=readonly -o "$scratch/admission" .
)
"$scratch/admission" "$policy" <"$controls"
printf 'GitHub team identity admission: %s controls submitted.\n' "$count"
