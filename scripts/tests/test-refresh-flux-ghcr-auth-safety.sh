#!/usr/bin/env bash

set -uo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly ROOT
readonly SAFETY_LIB="${ROOT}/scripts/refresh-flux-ghcr-auth-safety.sh"

failures=0

pass() {
  printf 'ok - %s\n' "$1"
}

fail() {
  printf 'not ok - %s\n' "$1"
  failures=$((failures + 1))
}

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

if [[ -r "${SAFETY_LIB}" ]]; then
  # shellcheck source=scripts/refresh-flux-ghcr-auth-safety.sh
  source "${SAFETY_LIB}"
else
  fail "production safety helpers exist"
fi

legacy_nodes="${work_dir}/legacy-nodes.json"
legacy_targets="${work_dir}/legacy-targets.tsv"
current_nodes="${work_dir}/current-nodes.json"
current_targets="${work_dir}/current-targets.tsv"
readonly DESIRED_REVISION="ciphertext-revision"
readonly DESIRED_IMAGE="ghcr.io/devantler-tech/ksail:v7.170.1"

jq -n \
  --arg revision "${DESIRED_REVISION}" \
  --arg image "${DESIRED_IMAGE}" '
  {
    items: [{
      metadata: {
        name: "prod-worker-1",
        labels: {},
        annotations: {
          "platform.devantler.tech/ghcr-pull-verified-revision": $revision,
          "platform.devantler.tech/ghcr-pull-verified-image": $image
        }
      },
      status: {addresses: [
        {type: "InternalIP", address: "10.0.0.4"}
      ]}
    }]
  }
' > "${legacy_nodes}"

if declare -F select_talos_node_targets >/dev/null; then
  if select_talos_node_targets \
    "${legacy_nodes}" \
    "${DESIRED_REVISION}" \
    "${DESIRED_IMAGE}" \
    "${legacy_targets}" \
    && [[ -s "${legacy_targets}" ]]; then
    pass "legacy verification markers force a one-time reboot"
  else
    fail "legacy verification markers force a one-time reboot"
  fi

  jq -n \
    --arg revision "${DESIRED_REVISION}" \
    --arg image "${DESIRED_IMAGE}" \
    --arg revision_key "${GHCR_PULL_VERIFIED_REVISION_ANNOTATION:-}" \
    --arg image_key "${GHCR_PULL_VERIFIED_IMAGE_ANNOTATION:-}" '
    {
      items: [{
        metadata: {
          name: "prod-worker-1",
          labels: {},
          annotations: {
            ($revision_key): $revision,
            ($image_key): $image
          }
        },
        status: {addresses: [
          {type: "InternalIP", address: "10.0.0.4"}
        ]}
      }]
    }
  ' > "${current_nodes}"

  if [[ "${GHCR_PULL_VERIFIED_REVISION_ANNOTATION:-}" == *-v2 ]] \
    && [[ "${GHCR_PULL_VERIFIED_IMAGE_ANNOTATION:-}" == *-v2 ]] \
    && select_talos_node_targets \
      "${current_nodes}" \
      "${DESIRED_REVISION}" \
      "${DESIRED_IMAGE}" \
      "${current_targets}" \
    && [[ ! -s "${current_targets}" ]]; then
    pass "v2 post-reboot markers suppress an already-proved reboot"
  else
    fail "v2 post-reboot markers suppress an already-proved reboot"
  fi
else
  fail "legacy verification markers force a one-time reboot"
  fail "v2 post-reboot markers suppress an already-proved reboot"
fi

operation_log="${work_dir}/operations.log"
patch_variables_base() {
  printf '%s\n' variables-patch >> "${operation_log}"
}
force_sync_resource() {
  printf 'force:%s/%s/%s\n' "$1" "$2" "$3" >> "${operation_log}"
}
verify_consumer_secret() {
  printf 'verify:%s/ghcr-auth\n' "$1" >> "${operation_log}"
}
sync_talos_registry_auth() {
  printf 'talos:%s:%s\n' "$1" "$2" >> "${operation_log}"
}
patch_root_secret() {
  printf '%s\n' root-patch >> "${operation_log}"
}

if declare -F stage_fanout_before_talos >/dev/null; then
  : > "${operation_log}"
  stage_fanout_before_talos \
    "${DESIRED_REVISION}" \
    "${DESIRED_IMAGE}" \
    wedding-app ascoachingogvaner kyverno
  expected_operations="$(printf '%s\n' \
    variables-patch \
    force:pushsecret/flux-system/seed-ghcr \
    force:externalsecret/wedding-app/ghcr-auth \
    verify:wedding-app/ghcr-auth \
    force:externalsecret/ascoachingogvaner/ghcr-auth \
    verify:ascoachingogvaner/ghcr-auth \
    force:externalsecret/kyverno/ghcr-auth \
    verify:kyverno/ghcr-auth \
    "talos:${DESIRED_REVISION}:${DESIRED_IMAGE}" \
    root-patch)"
  if [[ "$(<"${operation_log}")" == "${expected_operations}" ]]; then
    pass "verified tenant fanout completes before any Talos drain"
  else
    fail "verified tenant fanout completes before any Talos drain"
  fi
else
  fail "verified tenant fanout completes before any Talos drain"
fi

control_plane_inventory="${work_dir}/control-planes.json"
jq -n '
  {
    items: [
      {
        metadata: {
          name: "prod-control-plane-1",
          labels: {"node-role.kubernetes.io/control-plane": ""}
        },
        status: {
          addresses: [{type: "InternalIP", address: "10.0.0.1"}],
          conditions: [{type: "Ready", status: "True"}]
        }
      },
      {
        metadata: {
          name: "prod-control-plane-2",
          labels: {"node-role.kubernetes.io/control-plane": ""}
        },
        status: {
          addresses: [{type: "InternalIP", address: "10.0.0.2"}],
          conditions: [{type: "Ready", status: "True"}]
        }
      },
      {
        metadata: {
          name: "prod-control-plane-3",
          labels: {"node-role.kubernetes.io/control-plane": ""}
        },
        status: {
          addresses: [{type: "InternalIP", address: "10.0.0.3"}],
          conditions: [{type: "Ready", status: "True"}]
        }
      }
    ]
  }
' > "${control_plane_inventory}"

kubectl() {
  cp "${control_plane_inventory}" /dev/stdout
}

talosctl() {
  local arguments=" $* "
  local node=""
  local previous=""
  local argument

  for argument in "$@"; do
    if [[ "${previous}" == "--nodes" ]]; then
      node="${argument}"
    fi
    previous="${argument}"
  done

  if [[ "${arguments}" == *" etcd status "* ]]; then
    if [[ "${node}" == "${ETCD_STATUS_FAIL_NODE:-}" ]]; then
      return 1
    fi
    printf 'NODE MEMBER DB-SIZE\n%s member-id 1MB\n' "${node}"
    return 0
  fi

  if [[ "${arguments}" == *" etcd alarm list "* ]]; then
    printf 'NODE MEMBER ALARM\n'
    if [[ "${node}" == "${ETCD_ALARM_NODE:-}" ]]; then
      printf '%s member-id NOSPACE\n' "${node}"
    fi
    return 0
  fi

  return 64
}

if declare -F other_control_planes_safe_to_reboot >/dev/null; then
  ETCD_STATUS_FAIL_NODE=""
  ETCD_ALARM_NODE=""
  if other_control_planes_safe_to_reboot \
    prod-control-plane-1 test-context "${work_dir}" >/dev/null; then
    pass "healthy alarm-free etcd peers permit a control-plane reboot"
  else
    fail "healthy alarm-free etcd peers permit a control-plane reboot"
  fi

  ETCD_STATUS_FAIL_NODE="10.0.0.2"
  ETCD_ALARM_NODE=""
  if other_control_planes_safe_to_reboot \
    prod-control-plane-1 test-context "${work_dir}" >/dev/null 2>&1; then
    fail "unreadable etcd peer status blocks a control-plane reboot"
  else
    pass "unreadable etcd peer status blocks a control-plane reboot"
  fi

  ETCD_STATUS_FAIL_NODE=""
  ETCD_ALARM_NODE="10.0.0.3"
  if other_control_planes_safe_to_reboot \
    prod-control-plane-1 test-context "${work_dir}" >/dev/null 2>&1; then
    fail "an etcd peer alarm blocks a control-plane reboot"
  else
    pass "an etcd peer alarm blocks a control-plane reboot"
  fi
else
  fail "healthy alarm-free etcd peers permit a control-plane reboot"
  fail "unreadable etcd peer status blocks a control-plane reboot"
  fail "an etcd peer alarm blocks a control-plane reboot"
fi

if ((failures > 0)); then
  printf '%d safety regression test(s) failed\n' "${failures}" >&2
  exit 1
fi

printf 'All GHCR auth safety regression tests passed.\n'
