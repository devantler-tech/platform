#!/usr/bin/env bash
# The Flux-managed, non-datapath kube-system add-ons declare their CPU limit in the
# chart values, so a fresh or rebuilt cluster no longer depends on the kube-system
# LimitRange existing before the pod is admitted (#3789).
#
# Each target needs BOTH a CPU request and the CPU limit. auto-vpa controls requests
# and limits and keeps their ratio, and Kubernetes defaults a missing request to the
# limit: a limit declared alone would turn the admitted 15m:2 pair into 2:2, and VPA
# would then pin the container's limit at its ~50m request. The request is what keeps
# the live values unchanged, so it is asserted here as well.
#
# The datapath DaemonSets (the Cilium agent and cilium-envoy) are deliberately NOT
# limited here; that decision belongs to #3790, so this test fails if one gains a
# limit through these values.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root

readonly cilium='k8s/bases/infrastructure/controllers/cilium/helm-release.yaml'
readonly metrics='k8s/bases/infrastructure/controllers/metrics-server/helm-release.yaml'
readonly ccm='k8s/providers/hetzner/infrastructure/controllers/hcloud-ccm/helm-release.yaml'
readonly csi='k8s/providers/hetzner/infrastructure/controllers/hcloud-csi/helm-release.yaml'
readonly snapshot='k8s/providers/hetzner/infrastructure/controllers/snapshot-controller/helm-release.yaml'

# file|yq path to a resources block inside .spec.values
readonly targets="${cilium}|.spec.values.operator.resources
${cilium}|.spec.values.hubble.relay.resources
${cilium}|.spec.values.hubble.ui.frontend.resources
${cilium}|.spec.values.hubble.ui.backend.resources
${metrics}|.spec.values.resources
${ccm}|.spec.values.resources
${csi}|.spec.values.controller.resources.csiAttacher
${csi}|.spec.values.controller.resources.csiResizer
${csi}|.spec.values.controller.resources.csiProvisioner
${csi}|.spec.values.controller.resources.livenessProbe
${csi}|.spec.values.controller.resources.hcloudCSIDriver
${snapshot}|.spec.values.controller.resources"

readonly datapath="${cilium}|.spec.values.resources
${cilium}|.spec.values.envoy.resources"

# check <root> — prints one FAIL line per violation and returns non-zero on any.
check() {
  local root="$1" failures=0 line file path
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    file="${line%%|*}"
    path="${line#*|}"
    if [[ ! -f "${root}/${file}" ]]; then
      printf 'FAIL %s: file missing\n' "${file}"
      failures=$((failures + 1))
      continue
    fi
    if ! yq -e "(${path}.limits.cpu | tostring) == \"2\"" "${root}/${file}" >/dev/null 2>&1; then
      printf 'FAIL %s %s: limits.cpu must be "2"\n' "${file}" "${path}"
      failures=$((failures + 1))
    fi
    if ! yq -e "(${path}.requests.cpu // \"\" | tostring) != \"\"" "${root}/${file}" >/dev/null 2>&1; then
      printf 'FAIL %s %s: requests.cpu must be declared alongside the limit\n' "${file}" "${path}"
      failures=$((failures + 1))
    fi
  done <<<"${targets}"
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    file="${line%%|*}"
    path="${line#*|}"
    if ! yq -e "(${path}.limits // null) == null" "${root}/${file}" >/dev/null 2>&1; then
      printf 'FAIL %s %s: datapath limits are decided in #3790, not here\n' "${file}" "${path}"
      failures=$((failures + 1))
    fi
  done <<<"${datapath}"
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
  fail 'committed kube-system add-on values are missing a declared CPU limit or request'
fi

# 2-5. Each defect is caught, by name, on a copy of the tree. A control that could
#      fail for any other reason proves nothing, so every one asserts the exact path.
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT

expect_caught() { # <name> <file> <yq-edit> <expected-message-fragment>
  local name="$1" file="$2" edit="$3" want="$4" copy="${scratch}/$1"
  mkdir -p "${copy}"
  while IFS= read -r f; do
    [[ -n "${f}" ]] || continue
    mkdir -p "${copy}/$(dirname "${f}")"
    cp "${repo_root}/${f}" "${copy}/${f}"
  done < <(printf '%s\n%s\n' "${targets}" "${datapath}" | cut -d'|' -f1 | sort -u)
  yq -i "${edit}" "${copy}/${file}"
  if out="$(check "${copy}")"; then
    fail "${name}: the defect was not caught"
  fi
  [[ "${out}" == *"${want}"* ]] || fail "${name}: caught, but not for the expected reason: ${out}"
}

expect_caught limit-removed "${csi}" 'del(.spec.values.controller.resources.csiResizer.limits.cpu)' \
  'controller.resources.csiResizer: limits.cpu must be "2"'
expect_caught request-removed "${cilium}" 'del(.spec.values.hubble.relay.resources.requests)' \
  'hubble.relay.resources: requests.cpu must be declared'
expect_caught limit-changed "${metrics}" '.spec.values.resources.limits.cpu = "1"' \
  '.spec.values.resources: limits.cpu must be "2"'
expect_caught datapath-limited "${cilium}" '.spec.values.envoy.resources.limits.cpu = "2"' \
  '.spec.values.envoy.resources: datapath limits are decided in #3790'

printf 'kube-system add-on CPU limits: 12 containers declare request + limit, datapath untouched, 4 defects caught.\n'
