#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly crossview_dir="${root_dir}/k8s/bases/apps/crossview"
readonly cluster_reader="${root_dir}/k8s/bases/infrastructure/cluster-roles/cluster-reader.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly kubectl_guide="${root_dir}/docs/oidc-kubectl.md"
readonly talos_guide="${root_dir}/docs/talos-access.md"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

# Count the nodes a guard selects across EVERY document, treating any yq failure
# as a hard error.
#
# Two shapes are deliberately avoided here, because each one passes silently
# rather than failing when it breaks:
#
#   * `if yq eval -e '<expr>' >/dev/null 2>&1; then fail ...` cannot tell
#     "nothing matched" from "the expression did not parse" — both exit non-zero
#     and the redirect hides the message.
#   * `yq eval` reports one count PER DOCUMENT, so a multi-document render yields
#     a column of counts and any single-value test reads only the first.
#
# `eval-all` aggregates across documents, and a non-numeric result is fatal.
count_selected() {
  local expr="$1" file="$2" out
  out="$(yq eval-all "[${expr}] | length" "${file}" 2>&1)" ||
    fail "a boundary guard failed to evaluate against ${file}: ${out}"
  [[ "${out}" =~ ^[0-9]+$ ]] ||
    fail "a boundary guard did not yield a count against ${file}: ${out}"
  printf '%s' "${out}"
}

# Membership is expressed as list subtraction rather than `any_c(. == "a" or . ==
# "b")`: yq has no single-argument `any`, and inside `any_c` an `or` of two
# comparisons evaluates truthy for every input, so that guard matched everything
# it was pointed at while reading as a precise filter.
readonly secret_bearing_groups='["", "*"]'
readonly secret_bearing_resources='["secrets", "*"]'
readonly read_only_verbs='["get", "list", "watch"]'

readonly secret_grant_selector="
  select(
    (((.apiGroups // []) | length) != (((.apiGroups // []) - ${secret_bearing_groups}) | length)) and
    (((.resources // []) | length) != (((.resources // []) - ${secret_bearing_resources}) | length))
  )
"

grep -Fq "'scripts/tests/test-human-api-access-boundary.sh'" "${ci_workflow}" ||
  fail 'the human API access boundary test must trigger manifest validation'

grep -Fq 'run: bash scripts/tests/test-human-api-access-boundary.sh' "${ci_workflow}" ||
  fail 'CI must execute the human API access boundary test'

# Every file this test asserts over must also trigger it, or those assertions can
# be removed or drift without the gate ever running.
for boundary_input in 'docs/oidc-kubectl.md' 'docs/talos-access.md'; do
  grep -Fq "'${boundary_input}'" "${ci_workflow}" ||
    fail "${boundary_input} is asserted here, so it must also trigger this test"
done

rendered_manifests="$(mktemp)"
trap 'rm -f "${rendered_manifests}"' EXIT
kubectl kustomize "${crossview_dir}" >"${rendered_manifests}" ||
  fail 'the Crossview base must render'

yq eval -e '
  select(.kind == "HelmRelease" and .metadata.name == "crossview") |
  .spec.values.rbac.create == false
' "${rendered_manifests}" >/dev/null ||
  fail 'Crossview must disable the chart-owned wildcard ClusterRole'

# Disabling the chart's RBAC leaves its ServiceAccount in place — the chart gates
# that on `serviceAccount.create`, which stays true — so these bindings are what
# give the Crossview backend its read-only surface.
for binding in crossview-view crossview-cluster-reader; do
  expected_role="${binding#crossview-}"
  BINDING="${binding}" EXPECTED_ROLE="${expected_role}" yq eval -e '
    select(.kind == "ClusterRoleBinding" and .metadata.name == strenv(BINDING)) |
    .roleRef.kind == "ClusterRole" and
    .roleRef.name == strenv(EXPECTED_ROLE) and
    (.subjects | length) == 1 and
    .subjects[0].kind == "ServiceAccount" and
    .subjects[0].name == "crossview-sa" and
    .subjects[0].namespace == "crossview"
  ' "${rendered_manifests}" >/dev/null ||
    fail "${binding} must bind only crossview-sa to ${expected_role}"
done

[ "$(count_selected '
  select(.kind == "RoleBinding" or .kind == "ClusterRoleBinding") |
  .subjects[]? |
  select(.kind == "User")
' "${rendered_manifests}")" -eq 0 ] ||
  fail 'Crossview must not grant a human identity a port-forward bypass'

[ "$(count_selected "
  select(.kind == \"Role\" or .kind == \"ClusterRole\") |
  .rules[]? |
  ${secret_grant_selector}
" "${rendered_manifests}")" -eq 0 ] ||
  fail 'Crossview must not carry a direct Secret or wildcard core-resource grant'

[ "$(count_selected "
  .rules[] |
  ${secret_grant_selector}
" "${cluster_reader}")" -eq 0 ] ||
  fail 'cluster-reader must keep Secrets outside its read surface'

# Excluding Secrets is only half the boundary: both the daily OIDC identity and
# Crossview bind this role, so a rule gaining any mutating verb would breach it
# while still naming only safe resources.
[ "$(count_selected "
  .rules[] |
  select((((.verbs // []) - ${read_only_verbs}) | length) > 0)
" "${cluster_reader}")" -eq 0 ] ||
  fail 'cluster-reader must stay read-only: every rule may use only get, list and watch'

grep -Fq 'must contain only the OIDC reader context' "${kubectl_guide}" ||
  fail 'the kubectl guide must prohibit ambient break-glass credentials'

for required_oidc_arg in \
  '--exec-interactive-mode=IfAvailable' \
  '--exec-arg=--oidc-extra-scope=audience:server:client_id:public-client' \
  '--exec-arg=--token-cache-storage=keyring'; do
  grep -Fq -- "${required_oidc_arg}" "${kubectl_guide}" ||
    fail "the kubectl guide must include ${required_oidc_arg}"
done

grep -Fq 'os:reader' "${talos_guide}" ||
  fail 'the Talos guide must make os:reader the daily identity'

grep -Fq 'locked iCloud Note' "${talos_guide}" ||
  fail 'the Talos guide must name the trusted-device break-glass store'

printf 'PASS: Kubernetes and Talos daily identities are read-only and break-glass credentials stay offline\n'
