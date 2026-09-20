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

assert_event_writer() {
  local rendered="$1" scope="$2"

  if ! jq -e '
    [ .[]
      | select(.kind == "ClusterRole"
          and .metadata.name == "vpa-updater-event-writer")
    ] as $roles
    | [ .[]
        | select(.kind == "ClusterRoleBinding"
            and .metadata.name == "vpa-updater-event-writer")
      ] as $bindings
    | ($roles | length) == 1
      and ($bindings | length) == 1
      and ($roles[0].rules == [{
        apiGroups: [""],
        resources: ["events"],
        verbs: ["create", "patch", "update"]
      }])
      and ($bindings[0].roleRef == {
        apiGroup: "rbac.authorization.k8s.io",
        kind: "ClusterRole",
        name: "vpa-updater-event-writer"
      })
      and ($bindings[0].subjects == [{
        kind: "ServiceAccount",
        name: "vertical-pod-autoscaler-vpa-updater",
        namespace: "vertical-pod-autoscaler"
      }])
  ' "${rendered}" >/dev/null; then
    fail "${scope}: VPA updater must receive only create/patch/update on core Events through one exact ClusterRoleBinding"
  fi
}

for scope in base prod; do
  case "${scope}" in
    base) render_path="k8s/bases/infrastructure/controllers/vertical-pod-autoscaler" ;;
    prod) render_path="k8s/providers/hetzner/infrastructure/controllers" ;;
  esac
  kubectl kustomize "${root_dir}/${render_path}" >"${rendered_dir}/${scope}.yaml" ||
    fail "${scope}: the VPA controller manifests must render"
  yq -o=json '.' "${rendered_dir}/${scope}.yaml" | jq -s '.' >"${rendered_dir}/${scope}.json"
  assert_event_writer "${rendered_dir}/${scope}.json" "${scope}"
done

echo 'The VPA updater can maintain Events cluster-wide without unrelated authority.'
