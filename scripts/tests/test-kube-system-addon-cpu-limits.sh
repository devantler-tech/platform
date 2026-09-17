#!/usr/bin/env bash
# The Flux-managed kube-system add-ons, including the node datapath DaemonSets,
# declare their CPU limit in the chart values, so a fresh or rebuilt cluster does not
# depend on the kube-system LimitRange existing before the pod is admitted (#3789,
# #3790).
#
# Each target pins BOTH its CPU request and the CPU limit. The static prod system VPAs
# control requests and limits and keep their ratio, and Kubernetes defaults a missing
# request to the limit. A limit declared alone, or with the request raised to the
# limit, turns the admitted 15m:2 pair into 2:2, and the VPA then pins the container's
# limit at its ~50m request. The request value is what keeps the live values
# unchanged, so it is asserted exactly.
#
# Scope: this reads the HelmRelease values in the base and hetzner provider files. It
# does not render overlays, so an overlay patch that overrides these values is not
# seen here.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root

readonly cilium='k8s/bases/infrastructure/controllers/cilium/helm-release.yaml'
readonly tetragon='k8s/bases/infrastructure/controllers/tetragon/helm-release.yaml'
readonly metrics='k8s/bases/infrastructure/controllers/metrics-server/helm-release.yaml'
readonly ccm='k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/helm-release.yaml'
readonly csi='k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-release.yaml'
readonly snapshot='k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-release.yaml'

# file|yq path to a resources block inside .spec.values|expected requests.cpu
readonly targets="${cilium}|.spec.values.operator.resources|100m
${cilium}|.spec.values.hubble.relay.resources|15m
${cilium}|.spec.values.hubble.ui.frontend.resources|15m
${cilium}|.spec.values.hubble.ui.backend.resources|15m
${cilium}|.spec.values.resources|200m
${cilium}|.spec.values.envoy.resources|50m
${tetragon}|.spec.values.tetragon.resources|100m
${tetragon}|.spec.values.export.resources|15m
${metrics}|.spec.values.resources|100m
${ccm}|.spec.values.resources|100m
${csi}|.spec.values.controller.resources.csiAttacher|15m
${csi}|.spec.values.controller.resources.csiResizer|15m
${csi}|.spec.values.controller.resources.csiProvisioner|15m
${csi}|.spec.values.controller.resources.livenessProbe|15m
${csi}|.spec.values.controller.resources.hcloudCSIDriver|15m
${csi}|.spec.values.node.resources.csiNodeDriverRegistrar|15m
${csi}|.spec.values.node.resources.livenessProbe|15m
${csi}|.spec.values.node.resources.hcloudCSIDriver|15m
${snapshot}|.spec.values.controller.resources|10m"

# check <root> — prints one FAIL line per violation and returns non-zero on any.
check() {
  local root="$1" failures=0 line file rest path want
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    file="${line%%|*}"
    rest="${line#*|}"
    path="${rest%%|*}"
    want="${rest#*|}"
    if [[ ! -f "${root}/${file}" ]]; then
      printf 'FAIL %s: file missing\n' "${file}"
      failures=$((failures + 1))
      continue
    fi
    if ! yq -e "(${path}.limits.cpu | tostring) == \"2\"" "${root}/${file}" >/dev/null 2>&1; then
      printf 'FAIL %s %s: limits.cpu must be "2"\n' "${file}" "${path}"
      failures=$((failures + 1))
    fi
    if ! yq -e "(${path}.requests.cpu // \"\" | tostring) == \"${want}\"" "${root}/${file}" >/dev/null 2>&1; then
      printf 'FAIL %s %s: requests.cpu must be %s to keep the admitted ratio\n' "${file}" "${path}" "${want}"
      failures=$((failures + 1))
    fi
  done <<<"${targets}"
  [[ "${failures}" -eq 0 ]]
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq is required'

# 1. The committed tree passes.
if ! out="$(check "${repo_root}")"; then
  printf '%s\n' "${out}" >&2
  fail 'committed kube-system add-on values are missing a declared CPU limit or the expected request'
fi

# 2-9. Each defect is caught, by name, on a copy of the tree. A control that could
#      fail for any other reason proves nothing, so every one asserts the exact file
#      and path.
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT

expect_caught() { # <name> <file> <yq-edit> <expected-message-fragment>
  local name="$1" file="$2" edit="$3" want="$4" copy="${scratch}/$1" f
  mkdir -p "${copy}"
  while IFS= read -r f; do
    [[ -n "${f}" ]] || continue
    mkdir -p "${copy}/$(dirname "${f}")"
    cp "${repo_root}/${f}" "${copy}/${f}"
  done < <(printf '%s\n' "${targets}" | cut -d'|' -f1 | sort -u)
  yq -i "${edit}" "${copy}/${file}"
  if out="$(check "${copy}")"; then
    fail "${name}: the defect was not caught"
  fi
  [[ "${out}" == *"${want}"* ]] || fail "${name}: caught, but not for the expected reason: ${out}"
}

expect_caught limit-removed "${csi}" 'del(.spec.values.controller.resources.csiResizer.limits.cpu)' \
  "${csi} .spec.values.controller.resources.csiResizer: limits.cpu must be \"2\""
expect_caught request-removed "${cilium}" 'del(.spec.values.hubble.relay.resources.requests)' \
  "${cilium} .spec.values.hubble.relay.resources: requests.cpu must be 15m"
expect_caught request-raised-to-limit "${csi}" '.spec.values.controller.resources.livenessProbe.requests.cpu = "2"' \
  "${csi} .spec.values.controller.resources.livenessProbe: requests.cpu must be 15m"
expect_caught limit-changed "${metrics}" '.spec.values.resources.limits.cpu = "1"' \
  "${metrics} .spec.values.resources: limits.cpu must be \"2\""
expect_caught agent-limit-removed "${cilium}" 'del(.spec.values.resources.limits)' \
  "${cilium} .spec.values.resources: limits.cpu must be \"2\""
expect_caught envoy-limit-removed "${cilium}" 'del(.spec.values.envoy.resources.limits)' \
  "${cilium} .spec.values.envoy.resources: limits.cpu must be \"2\""
expect_caught tetragon-export-request-removed "${tetragon}" 'del(.spec.values.export.resources.requests)' \
  "${tetragon} .spec.values.export.resources: requests.cpu must be 15m"
expect_caught csi-node-limit-removed "${csi}" 'del(.spec.values.node.resources.hcloudCSIDriver.limits)' \
  "${csi} .spec.values.node.resources.hcloudCSIDriver: limits.cpu must be \"2\""

printf 'kube-system add-on CPU limits: 19 containers pin request + limit, 8 defects caught.\n'
