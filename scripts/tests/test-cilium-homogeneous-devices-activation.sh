#!/usr/bin/env bash

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

extract_top_level_update_strategy() {
  awk '
    /^    updateStrategy:[[:space:]]*$/ {
      found = 1
      print
      next
    }
    found && /^    [^[:space:]]/ { exit }
    found { print }
    END { exit !found }
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

require_pattern() {
  local haystack="$1"
  local pattern="$2"
  local description="$3"

  grep -Eq -- "${pattern}" <<<"${haystack}" || fail "${description}"
}

reject_pattern() {
  local haystack="$1"
  local pattern="$2"
  local description="$3"

  if grep -Eq -- "${pattern}" <<<"${haystack}"; then
    fail "${description}"
  fi
}

readonly homogeneous_devices_pattern='^[[:space:]]*devices:[[:space:]]*(en[+] eth[+]|"en[+] eth[+]")[[:space:]]*$'
readonly private_devices_pattern='^[[:space:]]*devices:[[:space:]]*(enp7s0 eth1|"enp7s0 eth1")[[:space:]]*$'

for rendered_devices in 'devices: en+ eth+' 'devices: "en+ eth+"'; do
  require_pattern \
    "${rendered_devices}" \
    "${homogeneous_devices_pattern}" \
    'the device matcher must accept quoted and unquoted values'
done
for rendered_devices in 'devices: enp7s0 eth1' 'devices: "enp7s0 eth1"'; do
  require_pattern \
    "${rendered_devices}" \
    "${private_devices_pattern}" \
    'the private-only matcher must detect quoted and unquoted values'
done

awk '
  /^      - name: 🌐 Validate active Cilium device selection$/ {
    found_step = 1
    next
  }
  found_step && /^        if: needs\.changes\.outputs\.k8s == '\''true'\''$/ {
    found_gate = 1
    exit
  }
  found_step && /^      - name:/ {
    exit
  }
  END {
    exit !(found_step && found_gate)
  }
' "${ci_workflow}" ||
  fail 'the homogeneous-device workflow step must be gated to k8s changes'

read -r private_line homogeneous_line < <(
  awk '
    $0 == "  - cilium/components/private-nic-devices/" { private = NR }
    $0 == "  - cilium/components/homogeneous-devices/" { homogeneous = NR }
    END { print private, homogeneous }
  ' "${controllers_kustomization}"
)

[[ -n "${private_line}" ]] ||
  fail 'the private-NIC component must remain active during the stepped rollout'
[[ -n "${homogeneous_line}" ]] ||
  fail 'the production controllers overlay must activate homogeneous device selection'
((private_line < homogeneous_line)) ||
  fail 'the homogeneous device component must follow the private-NIC component so its values win'

production_release="$(kubectl kustomize "${controllers_dir}" | extract_cilium_release)" ||
  fail 'the production controllers render has no Cilium HelmRelease'
production_update_strategy="$(extract_top_level_update_strategy <<<"${production_release}")" ||
  fail 'the production Cilium HelmRelease has no top-level update strategy'

require_pattern \
  "${production_release}" \
  "${homogeneous_devices_pattern}" \
  'the active production render must select both public and private device families'
reject_pattern \
  "${production_release}" \
  "${private_devices_pattern}" \
  'the active production render must not retain the private-only device pin'
require_text \
  "${production_update_strategy}" \
  'rollingUpdate: null' \
  'the activation must clear the chart default rollingUpdate map'
require_text \
  "${production_update_strategy}" \
  'type: OnDelete' \
  'the activation must clear rollingUpdate while staging an operator-stepped OnDelete rollout'
require_text \
  "${production_release}" \
  $'encryption:\n      enabled: true\n      nodeEncryption: false' \
  'the activation must preserve the production encryption settings'
require_text \
  "${production_release}" \
  'type: wireguard' \
  'the activation must preserve WireGuard encryption'

printf 'PASS: production activates homogeneous Cilium devices behind an OnDelete rollout gate\n'
