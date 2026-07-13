#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"
readonly opt_in_fixture="${root_dir}/tests/cilium-bandwidth-manager-bbr"

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
      if (is_helm_release && is_cilium) {
        printf "%s", document
        found = 1
        exit
      }
      reset_document()
    }

    BEGIN { reset_document() }
    /^---[[:space:]]*$/ { emit_if_cilium(); next }
    {
      document = document $0 ORS
      if ($0 ~ /^kind:[[:space:]]*HelmRelease[[:space:]]*$/) {
        is_helm_release = 1
      }
      if ($0 ~ /^  name:[[:space:]]*cilium[[:space:]]*$/) {
        is_cilium = 1
      }
    }
    END {
      if (!found && is_helm_release && is_cilium) {
        printf "%s", document
        found = 1
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

readonly controllers_kustomization="${controllers_dir}/kustomization.yaml"
grep -Fxq '  # - cilium/components/bandwidth-manager-bbr/' "${controllers_kustomization}" ||
  fail 'the production controllers overlay must retain the documented opt-in reference'
if grep -Fxq '  - cilium/components/bandwidth-manager-bbr/' "${controllers_kustomization}"; then
  fail 'the production controllers overlay must keep the BBR component disabled by default'
fi

default_release="$(kubectl kustomize "${controllers_dir}" | extract_cilium_release)" ||
  fail 'the default production controllers render has no Cilium HelmRelease'
reject_text \
  "${default_release}" \
  'bandwidthManager:' \
  'the default production render unexpectedly enables the bandwidth manager'

opt_in_release="$(
  kubectl kustomize "${opt_in_fixture}" --load-restrictor LoadRestrictionsNone |
    extract_cilium_release
)" || fail 'the opt-in fixture render has no Cilium HelmRelease'

require_text \
  "${opt_in_release}" \
  $'bandwidthManager:\n      bbr: true\n      bbrHostNamespaceOnly: true\n      enabled: true' \
  'the opt-in render must enable host-namespace-only BBR'
reject_text \
  "${opt_in_release}" \
  $'bpf:\n      masquerade: true' \
  'the opt-in render must not enable BPF masquerading'
require_text \
  "${opt_in_release}" \
  $'encryption:\n      enabled: true\n      nodeEncryption: false' \
  'the opt-in render must preserve the production encryption settings'
require_text \
  "${opt_in_release}" \
  'type: wireguard' \
  'the opt-in render must preserve WireGuard encryption'

printf 'PASS: Cilium bandwidth manager is default-off and the opt-in render preserves production guards\n'
