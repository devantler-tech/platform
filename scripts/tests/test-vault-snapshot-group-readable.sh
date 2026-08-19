#!/usr/bin/env bash
# The off-cluster mirror must not depend on being the snapshot's OWNER.
#
# `bao operator raft snapshot save` creates the file at the process umask and it
# lands `-rw-------` (0600), owned by the snapshot container's UID. `minio/mc`
# bakes no matching passwd entry and mounts /snapshots readOnly, so today it can
# open the snapshot only because it happens to run the same UID. That coupling is
# what pins all three vault-snapshots writers to a low host UID (checkov
# CKV_K8S_40, deferred to #3202).
#
# Making the snapshot group-readable breaks the coupling: both containers already
# run runAsGroup/fsGroup 1000, so the mirror can read it as GROUP and the UIDs
# become free to move independently.
#
# NOTE ON MECHANISM: this must be an explicit chmod, not a umask. umask can only
# CLEAR permission bits, never add them — so if `bao` requests 0600 explicitly (the
# observed 0600 under a normal 0022 umask says it does), lowering the umask is a
# silent no-op. chmod is correct under either hypothesis.
#
# The assertions below are deliberately BOUND to one another: each manifest's
# chmod must name the SAME path the snapshot was saved to. Checking "a save
# exists" and "a chmod exists" independently would pass while the chmod targeted
# some other file.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to read the snapshot container script'

# manifest:yq-path-to-the-snapshot-container-script
readonly targets=(
  "k8s/bases/infrastructure/vault-backup/job.yaml:.spec.template.spec.initContainers[]|select(.name==\"snapshot\")|.command[-1]"
  "k8s/bases/infrastructure/vault-backup/cron-job.yaml:.spec.jobTemplate.spec.template.spec.initContainers[]|select(.name==\"snapshot\")|.command[-1]"
)

for target in "${targets[@]}"; do
  manifest="${target%%:*}"
  query="${target#*:}"
  path="${root_dir}/${manifest}"

  [ -f "${path}" ] || fail "${manifest}: not found"

  script="$(yq "${query}" "${path}")"
  if [ -z "${script}" ] || [ "${script}" = "null" ]; then
    fail "${manifest}: could not read the snapshot container's script"
  fi

  # The save operand is the binding key. More than one save makes "the snapshot"
  # ambiguous, so refuse rather than guess which one the chmod should match.
  save_count="$(printf '%s\n' "${script}" | grep -c 'raft snapshot save' || true)"
  [ "${save_count}" -eq 1 ] ||
    fail "${manifest}: expected exactly 1 'raft snapshot save', found ${save_count} — binding is ambiguous"

  save_operand="$(printf '%s\n' "${script}" |
    sed -n 's/.*raft snapshot save[[:space:]]*//p' |
    head -1 |
    tr -d '"'"'"'')"
  [ -n "${save_operand}" ] || fail "${manifest}: could not extract the snapshot save operand"

  # Now require a chmod naming that SAME operand.
  chmod_line="$(printf '%s\n' "${script}" | grep -E '^[[:space:]]*chmod[[:space:]]' || true)"
  [ -n "${chmod_line}" ] ||
    fail "${manifest}: the snapshot script never chmods the snapshot; the mirror still depends on owning it (#3202)"

  # Bind the chmod side too. Checking only the FIRST chmod would let a later,
  # narrowing `chmod 0600 "$SNAP"` re-close the file while this test still
  # passed on the earlier widening one — the same unbound-assertion trap the
  # save operand is guarded against above.
  chmod_count="$(printf '%s\n' "${chmod_line}" | grep -c . || true)"
  [ "${chmod_count}" -eq 1 ] ||
    fail "${manifest}: expected exactly 1 chmod in the snapshot script, found ${chmod_count} — the final mode is ambiguous"

  chmod_mode="$(printf '%s\n' "${chmod_line}" | head -1 | awk '{print $2}')"
  chmod_operand="$(printf '%s\n' "${chmod_line}" | head -1 | awk '{print $3}' | tr -d '"'"'"'')"

  [ "${chmod_operand}" = "${save_operand}" ] ||
    fail "${manifest}: chmod targets '${chmod_operand}' but the snapshot is saved to '${save_operand}' — the assertions are unbound"

  # Group must gain read; world must gain nothing (the snapshot is vault data).
  case "${chmod_mode}" in
    [0-7][0-7][0-7] | [0-7][0-7][0-7][0-7]) ;;
    *) fail "${manifest}: chmod mode '${chmod_mode}' is not a 3- or 4-digit octal mode" ;;
  esac
  group_digit="${chmod_mode: -2:1}"
  other_digit="${chmod_mode: -1}"
  [ $((group_digit & 4)) -eq 4 ] ||
    fail "${manifest}: chmod mode '${chmod_mode}' does not grant GROUP read — the mirror still needs to be the owner"
  [ "${other_digit}" -eq 0 ] ||
    fail "${manifest}: chmod mode '${chmod_mode}' grants OTHER access to a vault snapshot"

  printf 'ok: %s — snapshot saved to %s and chmod %s applied to the same path\n' \
    "${manifest}" "${save_operand}" "${chmod_mode}"
done

printf 'PASS: both vault-snapshot writers make the snapshot group-readable\n'
