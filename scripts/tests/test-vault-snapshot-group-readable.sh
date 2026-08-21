#!/usr/bin/env bash
# The off-cluster mirror reads the snapshot as GROUP, not as its OWNER.
#
# `bao` creates the snapshot `-rw-------` (0600), owned by the snapshot
# container's UID. The kernel decides file access by comparing numeric UID and
# GID values, so an owner-only mode would force the mirror to run the same UID as
# the writer. Breaking that coupling is what lets the pods default to the high,
# unprivileged 65532 and scope UID 100 to the one container whose image bakes it
# (checkov CKV_K8S_40).
#
# Every container runs runAsGroup/fsGroup 1000, so a group-readable snapshot is
# readable by the mirror on its GROUP entry alone — a pod-level property, which is
# why this does not depend on the volume's setgid bit.
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

  # Comments are prose, not commands: a line that merely MENTIONS a save or a
  # chmod must not count toward either binding, or adding a note beside the
  # command would fail the test while the script itself was still correct.
  commands="$(printf '%s\n' "${script}" | grep -vE '^[[:space:]]*#' || true)"

  # The save operand is the binding key. More than one save makes "the snapshot"
  # ambiguous, so refuse rather than guess which one the chmod should match.
  save_count="$(printf '%s\n' "${commands}" | grep -c 'raft snapshot save' || true)"
  [ "${save_count}" -eq 1 ] ||
    fail "${manifest}: expected exactly 1 'raft snapshot save', found ${save_count} — binding is ambiguous"

  save_operand="$(printf '%s\n' "${commands}" |
    sed -n 's/.*raft snapshot save[[:space:]]*//p' |
    head -1 |
    tr -d '"'"'"'')"
  [ -n "${save_operand}" ] || fail "${manifest}: could not extract the snapshot save operand"

  # Now require a chmod naming that SAME operand.
  chmod_lines="$(printf '%s\n' "${commands}" | grep -E '^[[:space:]]*chmod[[:space:]]' || true)"
  [ -n "${chmod_lines}" ] ||
    fail "${manifest}: the snapshot script never chmods the snapshot; the mirror still depends on owning it (#3202)"

  # EVERY chmod must WIDEN, never narrow — asserted directly rather than by count.
  #
  # This used to require exactly ONE chmod, as a proxy for the same property: with
  # only one, a later narrowing `chmod 0600 "$SNAP"` could not exist to re-close the
  # file while this test still passed on the earlier widening one. The directory-wide
  # widen added for #3202 — every snapshot on the shared PVC, not only the newest —
  # needs a second chmod, so the proxy no longer fits.
  #
  # Checking the property itself is STRICTLY STRONGER, not a relaxation: the count
  # rule only ever constrained the SECOND chmod onwards and said nothing about the
  # mode of the lone one it permitted. This rejects a narrowing chmod wherever it
  # appears — including as the only one — and still refuses a symbolic or variable
  # mode it cannot read.
  bound=0
  chmod_modes=''
  while IFS= read -r chmod_line; do
    [ -n "${chmod_line}" ] || continue

    chmod_mode="$(printf '%s\n' "${chmod_line}" | awk '{print $2}')"
    chmod_operand="$(printf '%s\n' "${chmod_line}" | awk '{print $3}' | tr -d '"'"'"'')"

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

    if [ "${chmod_operand}" = "${save_operand}" ]; then
      bound=1
    fi
    chmod_modes="${chmod_modes:+${chmod_modes} }${chmod_mode}"
  done <<CHMODS
${chmod_lines}
CHMODS

  # Still bound: at least one chmod must name the SAME path the snapshot was saved
  # to, or the widening could be targeting some other file entirely.
  [ "${bound}" -eq 1 ] ||
    fail "${manifest}: no chmod targets the snapshot saved to '${save_operand}' — the assertions are unbound"

  printf 'ok: %s — snapshot saved to %s; chmod mode(s) %s, each group-readable and none world-readable\n' \
    "${manifest}" "${save_operand}" "${chmod_modes}"
done


# --- The UID split that group-readability makes possible ----------------------------------------
#
# Asserted here rather than in a separate file because it is the same property from the other end:
# the chmod above is only half of it. If the mirror were re-pinned to the openbao UID, every
# assertion above would still pass while the coupling it exists to prevent had quietly returned.

# manifest:yq-path-to-the-pod-spec
readonly uid_targets=(
  "k8s/bases/infrastructure/vault-backup/job.yaml:.spec.template.spec"
  "k8s/bases/infrastructure/vault-backup/cron-job.yaml:.spec.jobTemplate.spec.template.spec"
)

for target in "${uid_targets[@]}"; do
  manifest="${target%%:*}"
  pod="${target#*:}"
  path="${root_dir}/${manifest}"

  [ -f "${path}" ] || fail "${manifest}: not found"

  pod_uid="$(yq "${pod}.securityContext.runAsUser" "${path}")"
  pod_gid="$(yq "${pod}.securityContext.runAsGroup" "${path}")"
  pod_fsg="$(yq "${pod}.securityContext.fsGroup" "${path}")"

  case "${pod_uid}" in
    '' | null) fail "${manifest}: the pod sets no runAsUser" ;;
    *[!0-9]*) fail "${manifest}: pod runAsUser '${pod_uid}' is not numeric" ;;
  esac
  [ "${pod_uid}" -ge 10000 ] ||
    fail "${manifest}: pod runAsUser ${pod_uid} is a low host UID (CKV_K8S_40) — the writers are re-coupled"

  # The mirror reads on THIS group. If either value moves, group access stops working and the only
  # remaining path to the file is ownership, which is the coupling being removed.
  [ "${pod_gid}" = "1000" ] ||
    fail "${manifest}: pod runAsGroup is '${pod_gid}', not 1000 — the mirror loses its group entry"
  [ "${pod_fsg}" = "1000" ] ||
    fail "${manifest}: pod fsGroup is '${pod_fsg}', not 1000 — the mirror loses its group entry"

  snap_uid="$(yq "${pod}.initContainers[]|select(.name==\"snapshot\")|.securityContext.runAsUser" "${path}")"
  [ "${snap_uid}" = "100" ] ||
    fail "${manifest}: the snapshot container's runAsUser is '${snap_uid}', not the image's baked 100"

  # Asserted ABSENT rather than equal to the pod default: an explicit value here is precisely how
  # the mirror would be re-pinned to the writer's UID.
  mirror_uid="$(yq "${pod}.containers[]|select(.name==\"mirror\")|.securityContext.runAsUser" "${path}")"
  [ "${mirror_uid}" = "null" ] ||
    fail "${manifest}: the mirror pins runAsUser '${mirror_uid}' instead of taking the pod's high default"

  printf 'ok: %s — pod %s:%s (fsGroup %s); UID 100 scoped to the snapshot container; mirror takes the default\n' \
    "${manifest}" "${pod_uid}" "${pod_gid}" "${pod_fsg}"
done
printf 'PASS: both vault-snapshot writers make the snapshot group-readable, and the UID split holds\n'
