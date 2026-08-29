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

grep -Fq "'scripts/tests/test-human-api-access-boundary.sh'" "${ci_workflow}" ||
  fail 'the human API access boundary test must trigger manifest validation'

grep -Fq 'run: bash scripts/tests/test-human-api-access-boundary.sh' "${ci_workflow}" ||
  fail 'CI must execute the human API access boundary test'

rendered="$(kubectl kustomize "${crossview_dir}")" ||
  fail 'the Crossview base must render'

yq eval -e '
  select(.kind == "HelmRelease" and .metadata.name == "crossview") |
  .spec.values.rbac.create == false
' - <<<"${rendered}" >/dev/null ||
  fail 'Crossview must disable the chart-owned wildcard ClusterRole'

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
  ' - <<<"${rendered}" >/dev/null ||
    fail "${binding} must bind only crossview-sa to ${expected_role}"
done

if yq eval -e '
  select(.kind == "RoleBinding" or .kind == "ClusterRoleBinding") |
  .subjects[]? |
  select(.kind == "User")
' - <<<"${rendered}" >/dev/null 2>&1; then
  fail 'Crossview must not grant a human identity a port-forward bypass'
fi

if yq eval -e '
  select(.kind == "Role" or .kind == "ClusterRole") |
  .rules[]? |
  select(
    ((.apiGroups // []) | any(. == "" or . == "*")) and
    ((.resources // []) | any(. == "secrets" or . == "*"))
  )
' - <<<"${rendered}" >/dev/null 2>&1; then
  fail 'Crossview must not carry a direct Secret or wildcard core-resource grant'
fi

if yq eval -e '
  .rules[] |
  select(
    ((.apiGroups // []) | any(. == "" or . == "*")) and
    ((.resources // []) | any(. == "secrets" or . == "*"))
  )
' "${cluster_reader}" >/dev/null 2>&1; then
  fail 'cluster-reader must keep Secrets outside its read surface'
fi

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
