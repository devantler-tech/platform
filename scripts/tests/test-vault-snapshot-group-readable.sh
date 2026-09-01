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
# fsGroup 1000 gives every container supplementary group 1000. The snapshot container alone uses
# the openbao image's primary GID 1000; the mirror keeps the pod's high primary GID and reads the
# 0640 snapshot through that supplementary group. Process membership does not depend on the
# volume's setgid bit.
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
  #
  # `chmod` is a COMMAND, and a command is not the only thing that can start a line. An anchored
  # `^[[:space:]]*chmod[[:space:]]` match therefore checks a chmod only where it happens to be
  # written first — so `if chmod 0600 "$SNAP"; then`, `... && chmod 0600 "$B"`,
  # `... || chmod 0600 "$SNAP"`, `if ( chmod 0600 "$SNAP" ); then` and
  # `if { chmod 0600 "$SNAP"; }; then` are all INVISIBLE to it. That is the fail-open this guard
  # exists to prevent, one level down (#3268): a narrowing chmod hiding behind an `if` would
  # re-couple the mirror to file ownership with the guard still green. Found while working #3265,
  # where rewriting a widen as `if chmod 0640 "$EXISTING"; then` silently dropped the reported
  # modes from two to one.
  #
  # So the command text is normalised into one command per line BEFORE matching, in two passes.
  #
  # PASS 1 walks each line ONCE, carrying shell quote state, and is deliberately a single scan
  # rather than a comment-strip followed by a `gsub` split. Those are not equivalent: a `gsub`
  # cannot see quote state, so it splits a separator inside `echo 'note; chmod 0600 x'` and
  # invents a chmod out of a string literal; and a comment test that only accepts `#` after
  # whitespace misses `true;# note; chmod 0600 "$SNAP"`, where the comment opens straight after a
  # separator. Both are FALSE POSITIVES, and a guard that cries wolf on correct code is how a
  # guard gets ignored and then deleted. Walking once fixes both, because the same scan knows
  # whether it is inside quotes AND whether it is at a command boundary:
  #
  #   * a backslash escapes the next character outside single quotes (so `"a\"# b"` does not end
  #     the string early, which would truncate away a real chmod after it);
  #   * `'` and `"` toggle their quote state, and nothing inside a quote is a separator;
  #   * an UNQUOTED `;`, `&&`, `||` or `|` becomes a newline, so a command that FOLLOWS one starts
  #     its own line;
  #   * an UNQUOTED `#` ends the line, but only at a command boundary — nothing emitted yet, or
  #     the last thing emitted was whitespace or a separator. `a#b` is one word to the shell, and
  #     it is one word here too.
  #
  # PASS 2 strips what can still precede a command once the separators are gone: leading shell
  # keywords, and the `(` / `{` grouping tokens, repeatedly so `if ( chmod ... )` reduces all the
  # way. Grouping is stripped only at the START of a segment, never mid-line, so an operand
  # carrying a bracket and arithmetic like `$((widened + 1))` is left intact.
  #
  # Only then is the anchor applied. Because each surviving line now STARTS with `chmod`, the
  # positional `$2`/`$3` mode and operand extraction below stays correct and unchanged — the fix
  # is in what counts as a chmod, not in how one is read. Splitting also means a line carrying TWO
  # of them (`chmod 0640 "$A" && chmod 0600 "$B"`) contributes both, where the old matcher saw one.
  chmod_lines="$(printf '%s\n' "${commands}" |
    awk '
      {
        line = $0
        out = ""
        sq = 0
        dq = 0
        n = length(line)
        i = 1
        while (i <= n) {
          c = substr(line, i, 1)
          if (c == "\\" && sq == 0) { out = out c substr(line, i + 1, 1); i += 2; continue }
          if (c == "\047" && dq == 0) { sq = 1 - sq; out = out c; i++; continue }
          if (c == "\"" && sq == 0) { dq = 1 - dq; out = out c; i++; continue }
          if (sq == 0 && dq == 0) {
            if (c == "#") {
              last = (out == "") ? "" : substr(out, length(out), 1)
              if (out == "" || last == " " || last == "\t" || last == "\n") break
            }
            if (c == ";") { out = out "\n"; i++; continue }
            if (c == "&" && substr(line, i + 1, 1) == "&") { out = out "\n"; i += 2; continue }
            if (c == "|" && substr(line, i + 1, 1) == "|") { out = out "\n"; i += 2; continue }
            if (c == "|") { out = out "\n"; i++; continue }
          }
          out = out c
          i++
        }
        print out
      }' |
    awk '
      {
        line = $0
        sub(/^[ \t]+/, "", line)
        while (match(line, /^((if|then|else|elif|do|while|until|!|time|exec|command|eval)[ \t]+|[({][ \t]*)/)) {
          line = substr(line, RLENGTH + 1)
          sub(/^[ \t]+/, "", line)
        }
        print line
      }' |
    grep -E '^[[:space:]]*chmod[[:space:]]' || true)"
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

  case "${pod_gid}" in
    '' | null) fail "${manifest}: the pod sets no runAsGroup" ;;
    *[!0-9]*) fail "${manifest}: pod runAsGroup '${pod_gid}' is not numeric" ;;
  esac
  [ "${pod_gid}" -ge 10001 ] ||
    fail "${manifest}: pod runAsGroup ${pod_gid} is a low host GID (KSV-0021) — the mirror should use fsGroup 1000 only as a supplementary group"
  [ "${pod_fsg}" = "1000" ] ||
    fail "${manifest}: pod fsGroup is '${pod_fsg}', not 1000 — the mirror loses its group entry"

  snap_uid="$(yq "${pod}.initContainers[]|select(.name==\"snapshot\")|.securityContext.runAsUser" "${path}")"
  [ "${snap_uid}" = "100" ] ||
    fail "${manifest}: the snapshot container's runAsUser is '${snap_uid}', not the image's baked 100"

  # The group it WRITES with. runAsUser 100 alone is not enough: if this moved, the snapshot
  # would land with a group the mirror does not share, and chmod 0640 could not help it.
  snap_gid="$(yq "${pod}.initContainers[]|select(.name==\"snapshot\")|.securityContext.runAsGroup" "${path}")"
  [ "${snap_gid}" = "1000" ] ||
    fail "${manifest}: the snapshot container's runAsGroup is '${snap_gid}', not 1000 — it would write the snapshot with a group the mirror does not share"

  # Asserted ABSENT rather than equal to the pod default: an explicit value here is precisely how
  # the mirror would be re-pinned to the writer's UID.
  mirror_uid="$(yq "${pod}.containers[]|select(.name==\"mirror\")|.securityContext.runAsUser" "${path}")"
  [ "${mirror_uid}" = "null" ] ||
    fail "${manifest}: the mirror pins runAsUser '${mirror_uid}' instead of taking the pod's high default"

  mirror_gid="$(yq "${pod}.containers[]|select(.name==\"mirror\")|.securityContext.runAsGroup" "${path}")"
  [ "${mirror_gid}" = "null" ] ||
    fail "${manifest}: the mirror pins runAsGroup '${mirror_gid}' instead of taking the pod's high default and fsGroup supplementary membership"

  printf 'ok: %s — pod %s:%s (fsGroup %s); image UID/GID scoped to snapshot; mirror takes high defaults plus fsGroup\n' \
    "${manifest}" "${pod_uid}" "${pod_gid}" "${pod_fsg}"
done

# --- The DR fetch writes to the same volume, and it is the leg CI never exercises ---------------
#
# The restore reads the fetched snapshot as openbao (uid 100, gid 1000). The fetch pod now runs a
# high uid, so the group entry is the ONLY remaining path to that file — and mc's umask is not
# something this repository controls. Assert the explicit chmod instead.
#
# Scoped to chmods targeting /snapshots: this workflow also chmods an age private key to 600, which
# is correct and must not be read as a finding.
#
# Text-level rather than yq: the pod spec lives inside a heredoc within a `run:` block, so it is not
# addressable by a YAML path. Bound the same way as the manifests above — the chmod must name the
# SAME operand the fetch wrote to, or it could be widening some other file.
dr_workflow='.github/workflows/dr-rebuild.yaml'
dr_path="${root_dir}/${dr_workflow}"
[ -f "${dr_path}" ] || fail "${dr_workflow}: not found"

dr_cp="$(grep -E '^[[:space:]]*mc cp .*/snapshots/' "${dr_path}" || true)"
[ -n "${dr_cp}" ] || fail "${dr_workflow}: no 'mc cp' onto /snapshots — the fetch step moved or was renamed"
dr_operand="$(printf '%s\n' "${dr_cp}" | awk '{print $NF}' | tr -d '"')"
[ -n "${dr_operand}" ] || fail "${dr_workflow}: could not extract the fetch destination"

dr_chmod="$(grep -E '^[[:space:]]*chmod[[:space:]]+[0-7]+[[:space:]]+"?/snapshots/' "${dr_path}" || true)"
[ -n "${dr_chmod}" ] ||
  fail "${dr_workflow}: the fetch never chmods the snapshot; the restore depends on mc's umask"

dr_bound=0
while IFS= read -r line; do
  [ -n "${line}" ] || continue
  mode="$(printf '%s\n' "${line}" | awk '{print $2}')"
  operand="$(printf '%s\n' "${line}" | awk '{print $3}' | tr -d '"')"
  case "${mode}" in
    [0-7][0-7][0-7] | [0-7][0-7][0-7][0-7]) ;;
    *) fail "${dr_workflow}: chmod mode '${mode}' is not a 3- or 4-digit octal mode" ;;
  esac
  [ $((${mode: -2:1} & 4)) -eq 4 ] ||
    fail "${dr_workflow}: chmod mode '${mode}' does not grant GROUP read — the restore cannot open it"
  [ "${mode: -1}" -eq 0 ] ||
    fail "${dr_workflow}: chmod mode '${mode}' grants OTHER access to a vault snapshot"
  [ "${operand}" = "${dr_operand}" ] && dr_bound=1
done <<DRCHMODS
${dr_chmod}
DRCHMODS

[ "${dr_bound}" -eq 1 ] ||
  fail "${dr_workflow}: no chmod targets the fetched snapshot '${dr_operand}' — the assertion is unbound"

printf 'ok: %s — fetch writes %s and chmods that same path group-readable\n' "${dr_workflow}" "${dr_operand}"

# The fetch pod's own identity decides the GROUP of the file it writes, so the chmod above is only
# half of this leg too: a correct 0640 on a file whose group nobody shares is still unreadable.
# Bound to the pod's own block — the workflow sets securityContext elsewhere as well.
dr_pod="$(awk '/name: dr-snapshot-fetch/{f=1} f{print} f && /^[[:space:]]*containers:/{exit}' "${dr_path}")"
[ -n "${dr_pod}" ] || fail "${dr_workflow}: could not locate the dr-snapshot-fetch pod spec"

dr_field() { printf '%s\n' "${dr_pod}" | sed -n "s/^[[:space:]]*$1:[[:space:]]*//p" | head -1; }

dr_uid="$(dr_field runAsUser)"
dr_gid="$(dr_field runAsGroup)"
dr_fsg="$(dr_field fsGroup)"

case "${dr_uid}" in
  '') fail "${dr_workflow}: the fetch pod sets no runAsUser" ;;
  *[!0-9]*) fail "${dr_workflow}: fetch pod runAsUser '${dr_uid}' is not numeric" ;;
esac
[ "${dr_uid}" -ge 10000 ] ||
  fail "${dr_workflow}: fetch pod runAsUser ${dr_uid} is a low host UID (CKV_K8S_40) — re-coupled to the writer"
[ "${dr_gid}" = "1000" ] ||
  fail "${dr_workflow}: fetch pod runAsGroup is '${dr_gid}', not 1000 — the restore could not read what it fetched"
[ "${dr_fsg}" = "1000" ] ||
  fail "${dr_workflow}: fetch pod fsGroup is '${dr_fsg}', not 1000 — the restore could not read what it fetched"

printf 'ok: %s — fetch pod runs %s:%s (fsGroup %s)\n' "${dr_workflow}" "${dr_uid}" "${dr_gid}" "${dr_fsg}"
printf 'PASS: both vault-snapshot writers make the snapshot group-readable, and the UID split holds\n'
