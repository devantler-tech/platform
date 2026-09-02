#!/usr/bin/env bash
#
# Fail when a control suppressed by a ClusterSecurityException is not named in that
# file's fail-open warning block.
#
# #3516 exists because C-0211 was added to `pod-security-mutations-unscoped.yaml`
# under `spec.posture` while the warning block above it still spoke only about
# C-0013. That warning is the file's one record of a real hazard: the exception makes
# every control it suppresses report `status: passed` with `subStatus: "w/exceptions"`
# on `workloadconfigurationscans`, so reading `status` alone returns a clean result
# over a wholly unfixed population. For C-0211 that was 35 of 35 workloads.
#
# Nothing detected the omission. It is silent in both directions — nothing fails when
# the control is added undocumented, and the wrong reading it enables looks like a
# passing security result rather than an error. It was found by hand.
#
# 🔴 THE WHOLE HEADER IS NOT THE ANSWER — SCOPE TO THE WARNING BLOCK.
#
# When C-0211 was added it WAS mentioned elsewhere in the same header, in a section
# describing which rules it carries. A guard that accepts a mention anywhere in the
# file therefore passes on exactly the tree that motivated it, and would have to be
# rewritten the first time it was trusted. The block is delimited explicitly, by
# `fail-open-warning:BEGIN` / `:END` markers, rather than inferred from comment
# shape — an inferred boundary silently widens as the file is edited, which is the
# same failure one level up.
#
# 🔴 FAIL CLOSED. Anything this guard cannot read is exit 2, never a quiet pass. An
# empty control list compared against an empty block is the failure mode that makes a
# guard look green forever, so both are required to be non-empty before any
# comparison happens.
#
# Exit codes:
#   0  every suppressed control is named in the warning block
#   1  at least one is not — each is named on stderr
#   2  cannot check: bad usage, a missing/unreadable/unparseable file, markers that
#      are absent, duplicated or out of order, or either list coming back empty

set -euo pipefail

readonly BEGIN_MARK='fail-open-warning:BEGIN'
readonly END_MARK='fail-open-warning:END'
readonly DEFAULT_FILE='k8s/bases/infrastructure/cluster-security-exceptions/pod-security-mutations-unscoped.yaml'

me="$(basename "$0")"
die() {
  printf '%s: cannot check: %s\n' "${me}" "$*" >&2
  exit 2
}
fail() { printf '%s: %s\n' "${me}" "$*" >&2; }

[ "$#" -le 1 ] || die "usage: ${me} [<exception-file>]"
file="${1:-${DEFAULT_FILE}}"

[ -f "${file}" ] || die "no such file: ${file}"
[ -r "${file}" ] || die "unreadable: ${file}"
command -v yq >/dev/null 2>&1 || die "yq is not installed"

# --- the controls this exception suppresses -------------------------------------
# Read as YAML rather than by grep: `controlID` is a structural field, and a grep
# would also match the identifier wherever it appears in prose, including inside the
# very warning block this guard checks against — which would make the check circular.
kind="$(yq -r '.kind // ""' "${file}")" || die "yq failed to parse ${file}"
[ "${kind}" = "ClusterSecurityException" ] ||
  die "${file}: kind is '${kind}', expected ClusterSecurityException"

controls="$(yq -r '.spec.posture[].controlID' "${file}")" ||
  die "yq failed to read .spec.posture[].controlID from ${file}"
[ -n "${controls}" ] ||
  die "${file}: no controls under .spec.posture — refusing to vouch for an empty comparison"

# --- the warning block ----------------------------------------------------------
begin_n="$(grep -cF -- "${BEGIN_MARK}" "${file}")" || begin_n=0
end_n="$(grep -cF -- "${END_MARK}" "${file}")" || end_n=0
[ "${begin_n}" -eq 1 ] || die "${file}: expected exactly 1 '${BEGIN_MARK}' marker, found ${begin_n}"
[ "${end_n}" -eq 1 ] || die "${file}: expected exactly 1 '${END_MARK}' marker, found ${end_n}"

begin_line="$(grep -nF -- "${BEGIN_MARK}" "${file}" | cut -d: -f1)"
end_line="$(grep -nF -- "${END_MARK}" "${file}" | cut -d: -f1)"
[ "${begin_line}" -lt "${end_line}" ] ||
  die "${file}: '${BEGIN_MARK}' (line ${begin_line}) must precede '${END_MARK}' (line ${end_line})"

# Strictly between the markers: the marker lines themselves are structure, not prose,
# and naming a control on one of them must not count as documenting it.
block="$(awk -v a="${begin_line}" -v b="${end_line}" 'NR>a && NR<b' "${file}")" ||
  die "${file}: could not extract the warning block"
[ -n "${block}" ] ||
  die "${file}: the warning block is empty — refusing to vouch for an empty comparison"

# --- compare --------------------------------------------------------------------
missing=0
while IFS= read -r control; do
  [ -n "${control}" ] || continue
  case "${control}" in
    C-[0-9]*) ;;
    *) die "${file}: '${control}' is not a control id of the form C-<digits>" ;;
  esac
  # -w so C-0021 never satisfies a requirement to document C-0021x, and a bare
  # substring of a longer id never counts.
  # Capture grep's OWN status. `if cmd; then ...; fi` reports 0 when the condition is
  # false and no else branch runs, so reading $? after the fi asks the `if` how it
  # went, not the matcher — the same "status of the wrong thing" this guard's sibling
  # fix (#3504) exists to close.
  rc=0
  grep -qwF -- "${control}" <<<"${block}" || rc=$?
  if [ "${rc}" -eq 0 ]; then
    continue
  fi
  [ "${rc}" -eq 1 ] || die "${file}: grep failed (exit ${rc}) while looking for ${control}"
  fail "${control} is suppressed under .spec.posture but is not named in the fail-open warning block (lines ${begin_line}-${end_line})"
  missing=$((missing + 1))
done <<EOF
${controls}
EOF

if [ "${missing}" -gt 0 ]; then
  fail "${missing} suppressed control(s) undocumented in ${file}"
  fail "Add each to the block between '${BEGIN_MARK}' and '${END_MARK}', stating how that control fails open."
  exit 1
fi

printf '%s: ok — %s suppressed control(s), each named in the fail-open warning block of %s\n' \
  "${me}" "$(awk 'END { print NR }' <<<"${controls}")" "${file}"
