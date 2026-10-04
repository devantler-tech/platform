#!/usr/bin/env bash

# Keep the PDB-respecting Talos drain budget beyond CNPG's graceful stop window.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly cluster_config="${root_dir}/ksail.prod.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq is required'

drain_timeout="$(yq eval -r '.spec.cluster.talos.drainTimeout // ""' "${cluster_config}")"
case "${drain_timeout}" in
  *m)
    drain_minutes="${drain_timeout%m}"
    [[ "${drain_minutes}" =~ ^[0-9]+$ ]] || fail "invalid minute drainTimeout: ${drain_timeout}"
    drain_seconds=$((drain_minutes * 60))
    ;;
  *)
    fail "drainTimeout must be a finite whole-minute duration, got: ${drain_timeout:-<empty>}"
    ;;
esac

# CloudNativePG defaults stopDelay to 1800 seconds and uses that value for the
# pod's termination grace. Longhorn correctly holds its generated
# InstanceManager PDB until the attached engine disappears. The node drain must
# therefore outlive that grace by a finite recovery margin instead of racing it
# at the same 30-minute boundary.
readonly cnpg_default_stop_delay_seconds=1800
readonly detach_and_pdb_headroom_seconds=600
readonly minimum_drain_seconds=$((cnpg_default_stop_delay_seconds + detach_and_pdb_headroom_seconds))

((drain_seconds >= minimum_drain_seconds)) ||
  fail "drainTimeout ${drain_timeout} does not cover CNPG's 1800s stopDelay plus 600s Longhorn detach/PDB headroom"

printf 'ok — Talos drainTimeout %s covers CNPG grace plus Longhorn detach/PDB headroom\n' "${drain_timeout}"
