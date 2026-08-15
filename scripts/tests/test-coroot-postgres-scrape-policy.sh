#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production apps overlay'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the rendered Cilium policies'

rendered="$(kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps")" ||
  fail 'the production apps overlay must render successfully'

for namespace in backstage umami; do
  selectors="$(
    printf '%s\n' "${rendered}" |
      NAMESPACE="${namespace}" yq ea -o=json -I=0 '[
        select(
          (.kind == "CiliumNetworkPolicy") and
          (.metadata.name == "allow-coroot-postgres-scrape") and
          (.metadata.namespace == strenv(NAMESPACE))
        ) |
        .spec.ingress[].fromEndpoints[].matchLabels
      ]' -
  )" || fail "the rendered ${namespace} scrape policy must be inspectable"

  printf '%s\n' "${selectors}" |
    yq e -e '
      length == 1 and
      .[0]."k8s:io.kubernetes.pod.namespace" == "observability" and
      .[0]."app.kubernetes.io/managed-by" == "coroot-operator" and
      .[0]."app.kubernetes.io/part-of" == "coroot" and
      .[0]."app.kubernetes.io/component" == "coroot-cluster-agent" and
      (.[0] | length) == 4
    ' - >/dev/null ||
    fail "the rendered ${namespace} scrape policy must select only the Coroot cluster-agent"
done

printf 'PASS: Coroot Postgres scrape policies select only the generated cluster-agent labels\n'
