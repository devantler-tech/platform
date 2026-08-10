#!/usr/bin/env bash
# Assert the Kubescape posture gate still evaluates every framework the exception
# set depends on.
#
# WHY THIS EXISTS (#2823)
# The gate scanned `nsa` alone for a long time. The ClusterSecurityException CRs
# name 76 distinct controls; NSA-CISA evaluates 17 of them, so 59 excepted controls
# — including every RBAC control those CRs exist to govern — were never scored,
# never gated, and never sent to Code Scanning. Nothing failed. The score simply
# did not include them, which is why those exceptions read as "inert" for weeks.
#
# That is the failure mode this guard exists for: dropping a framework REMOVES
# findings, so the compliance score goes UP and every check stays green. A coverage
# regression here is indistinguishable from an improvement unless something asserts
# the framework list itself.
#
# WHAT THIS DOES NOT DO, AND WHY
# It does not re-run the scanner against fixture findings to re-derive the
# exit-code matrix. Two reasons. The threshold's pass/fail behaviour is ksail and
# kubescape's, not ours — testing it would pin a dependency's contract, and it is
# already exercised for real on every PR by the gate itself. And the aggregate
# score is environment-dependent (Linux runner vs macOS, and it shifts with the
# ksail render), so a fixture asserting concrete scores would flake or be re-pinned
# into meaninglessness. What is genuinely ours, and silently reversible, is WHICH
# FRAMEWORKS ARE NAMED. That is what this checks.
#
# The measured ablation behind the current configuration is recorded beside the
# step in ci.yaml: thresholds 95/99/100 give exit 0/1/1 against a combined score
# of 97.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT

readonly WORKFLOW='.github/workflows/ci.yaml'

# Every framework the gate must evaluate. `mitre` is here because it is the only
# framework that reaches C-0007, C-0015, C-0031, C-0037, C-0045, C-0048 and
# C-0053 — the excepted RBAC controls. Removing it silently un-gates all seven.
readonly REQUIRED_FRAMEWORKS=(nsa mitre)

main() {
  cd "$REPO_ROOT"

  if [ ! -f "$WORKFLOW" ]; then
    printf '::error::%s not found; nothing was validated\n' "$WORKFLOW" >&2
    exit 1
  fi

  # A floor, because an empty result from a filtered read is a claim about the
  # FILTER. If the step is renamed, moved into a composite action, or the flag is
  # spelled differently, this grep returns nothing — and without the floor the
  # guard would exit 0 having checked precisely nothing.
  # `mapfile` is bash 4+; macOS ships bash 3.2, so a contributor on a Mac would
  # get `command not found` (exit 127) while CI's Ubuntu bash passed. Read the
  # matches into the array the portable way instead.
  local -a scan_lines=()
  local match
  while IFS= read -r match; do
    [ -n "$match" ] && scan_lines+=("$match")
  done < <(grep -nE 'ksail workload scan[^|]*--framework[[:space:]]+[a-z,]+' "$WORKFLOW" || true)

  if [ "${#scan_lines[@]}" -eq 0 ]; then
    printf '::error::no "ksail workload scan --framework ..." invocation found in %s.\n' "$WORKFLOW" >&2
    printf '::error::The gate moved or was renamed. Point this guard at it rather than deleting the guard.\n' >&2
    exit 1
  fi

  local rc=0 line lineno frameworks fw
  for line in "${scan_lines[@]}"; do
    lineno="${line%%:*}"
    # The frameworks are the comma-separated token after `--framework`.
    frameworks="$(printf '%s' "$line" | sed -nE 's/.*--framework[[:space:]]+([a-z,]+).*/\1/p')"

    if [ -z "$frameworks" ]; then
      printf '::error file=%s,line=%s::could not read the --framework value.\n' "$WORKFLOW" "$lineno" >&2
      rc=1
      continue
    fi

    for fw in "${REQUIRED_FRAMEWORKS[@]}"; do
      # Match a whole comma-separated element, never a substring: `nsa` must not
      # be satisfied by some future `nsa-extended`.
      if ! printf ',%s,' "$frameworks" | grep -qF ",${fw},"; then
        printf '::error file=%s,line=%s::the Kubescape gate must evaluate "%s", but --framework is "%s".\n' \
          "$WORKFLOW" "$lineno" "$fw" "$frameworks" >&2
        printf '::error::Dropping a framework REMOVES findings, so the compliance score RISES and CI stays green. See #2823.\n' >&2
        rc=1
      fi
    done
  done

  if [ "$rc" -ne 0 ]; then
    exit 1
  fi

  printf 'Kubescape gate evaluates all %d required framework(s) across %d invocation(s).\n' \
    "${#REQUIRED_FRAMEWORKS[@]}" "${#scan_lines[@]}"
}

main "$@"
