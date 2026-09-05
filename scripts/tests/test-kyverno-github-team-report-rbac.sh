#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
rendered_dir="$(mktemp -d)"
trap 'rm -rf "${rendered_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

# Inspect the rules selected by each controller's aggregation label, rather
# than a source filename: an unreferenced or wrongly labelled grant gives the
# controller no access even when the grant's own rules look correct.
assert_team_reads() {
  local rendered="$1" controller="$2" scope="$3"

  if ! jq -e --arg label "rbac.kyverno.io/aggregate-to-${controller}-controller" '
    [ .[]
      | select(.kind == "ClusterRole" and .metadata.labels[$label] == "true")
      | select(any(.rules[]?.apiGroups[]?;
          . == "team.github.m.upbound.io" or . == "*"))
    ] as $roles
    | ($roles | length) > 0
      and all($roles[];
        .aggregationRule == null
        and all(.rules[];
          .apiGroups == ["team.github.m.upbound.io"]
          and ((.resources // []) - ["teams", "teammemberships", "teamrepositories"] | length) == 0
          and ((.verbs // []) - ["get", "list", "watch"] | length) == 0
          and ((.resourceNames // []) | length) == 0
          and ((.nonResourceURLs // []) | length) == 0))
      and ([ $roles[].rules[]
             | .resources[] as $resource
             | .verbs[]
             | [$resource, .]
           ] | unique | sort) ==
          ([ ["teams", "get"], ["teams", "list"], ["teams", "watch"],
             ["teammemberships", "get"], ["teammemberships", "list"], ["teammemberships", "watch"],
             ["teamrepositories", "get"], ["teamrepositories", "list"], ["teamrepositories", "watch"]
           ] | sort)
  ' "${rendered}" >/dev/null; then
    fail "${scope}: ${controller} controller must aggregate unrestricted get/list/watch on exactly the three GitHub team resources, with no additional authority"
  fi
}

for scope in base prod; do
  case "${scope}" in
    base) render_path="k8s/bases/infrastructure/controllers/kyverno" ;;
    prod) render_path="k8s/providers/hetzner/infrastructure/controllers" ;;
  esac
  kubectl kustomize "${root_dir}/${render_path}" >"${rendered_dir}/${scope}.yaml" ||
    fail "${scope}: the controller manifests must render"
  yq -o=json '.' "${rendered_dir}/${scope}.yaml" | jq -s '.' >"${rendered_dir}/${scope}.json"
  for controller in background reports; do
    assert_team_reads "${rendered_dir}/${scope}.json" "${controller}" "${scope}"
  done
done

echo 'Both Kyverno controllers can read GitHub team resources without receiving write or wildcard access.'
