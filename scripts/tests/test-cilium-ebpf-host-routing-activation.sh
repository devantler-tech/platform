#!/usr/bin/env bash
# Pins the production activation of the eBPF host-routing + BPF masquerade
# component (#4260): it is referenced, it is listed after the homogeneous
# device selection its masquerade program depends on, and the production
# Cilium render carries both values without enabling the bandwidth manager.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"
readonly controllers_kustomization="${controllers_dir}/kustomization.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

extract_cilium_release() {
  awk '
    function reset_document() {
      document = ""
      is_helm_release = 0
      is_cilium = 0
    }

    function emit_if_cilium() {
      if (!found && is_helm_release && is_cilium) {
        printf "%s", document
        found = 1
      }
      reset_document()
    }

    BEGIN { reset_document() }
    /^---[[:space:]]*$/ { emit_if_cilium(); next }
    {
      if (!found) {
        document = document $0 ORS
        if ($0 ~ /^kind:[[:space:]]*HelmRelease[[:space:]]*$/) {
          is_helm_release = 1
        }
        if ($0 ~ /^  name:[[:space:]]*cilium[[:space:]]*$/) {
          is_cilium = 1
        }
      }
    }
    END {
      if (!found) {
        emit_if_cilium()
      }
      if (!found) {
        exit 1
      }
    }
  '
}

require_text() {
  local haystack="$1"
  local needle="$2"
  local description="$3"

  grep -Fq -- "$needle" <<<"${haystack}" || fail "${description}"
}

reject_text() {
  local haystack="$1"
  local needle="$2"
  local description="$3"

  if grep -Fq -- "$needle" <<<"${haystack}"; then
    fail "${description}"
  fi
}

grep -Fq 'bash scripts/tests/test-cilium-ebpf-host-routing-activation.sh' "${ci_workflow}" ||
  fail 'CI must run the eBPF host-routing activation test'

ebpf_line="$(grep -nFx '  - cilium/components/ebpf-host-routing/' "${controllers_kustomization}" | cut -d: -f1 || true)"
devices_line="$(grep -nFx '  - cilium/components/homogeneous-devices/' "${controllers_kustomization}" | cut -d: -f1 || true)"
[ -n "${ebpf_line}" ] ||
  fail 'the production controllers overlay must activate eBPF host routing'
[ -n "${devices_line}" ] ||
  fail 'eBPF host routing needs the homogeneous device selection to be active'
[ "${ebpf_line}" -gt "${devices_line}" ] ||
  fail 'eBPF host routing must be listed after the homogeneous device selection'

production_release="$(kubectl kustomize "${controllers_dir}" | extract_cilium_release)" ||
  fail 'the production controllers render has no Cilium HelmRelease'

require_text \
  "${production_release}" \
  $'bpf:\n      hostLegacyRouting: false\n      masquerade: true' \
  'the production render must enable BPF masquerading and eBPF host routing'
require_text \
  "${production_release}" \
  'devices: en+ eth+' \
  'the production render must keep the wildcard device selection masquerade attaches to'
require_text \
  "${production_release}" \
  'kubeProxyReplacement: true' \
  'BPF masquerading requires kube-proxy replacement'
require_text \
  "${production_release}" \
  'type: wireguard' \
  'the production render must preserve WireGuard encryption'
reject_text \
  "${production_release}" \
  'bandwidthManager:' \
  'the bandwidth manager must stay a separate, later activation'

printf 'PASS: eBPF host routing and BPF masquerade are active after the homogeneous device selection\n'
