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

# BOTH workflows, and that is the point rather than thoroughness. They upload
# under the SAME Code Scanning category, so validate-main.yaml's run is the
# durable main-branch baseline that ci.yaml's PR alerts are diffed against. If
# the two scan different frameworks, findings only the PR sees never persist as
# a main-branch alert, and a direct push to main — which bypasses the merge
# queue — goes ungated on whatever the baseline omits. Caught by Codex on #3057
# after the first version of this guard checked ci.yaml alone.
readonly WORKFLOWS=(
  '.github/workflows/ci.yaml'
  '.github/workflows/validate-main.yaml'
)

# Every framework the gate must evaluate. `mitre` is here because it is the only
# framework that reaches C-0007, C-0015, C-0031, C-0037, C-0045, C-0048 and
# C-0053 — the excepted RBAC controls. Removing it silently un-gates all seven.
readonly REQUIRED_FRAMEWORKS=(nsa mitre)

main() {
  cd "$REPO_ROOT"

  local rc=0 workflow
  for workflow in "${WORKFLOWS[@]}"; do
    check_workflow "$workflow" || rc=1
  done

  # The required members are a FLOOR; the workflows must also agree EXACTLY.
  # Both upload under one Code Scanning category, so any framework present in
  # one and absent from the other is findings that appear on PRs but never
  # persist on main, or a framework main gates on that PRs never see. Checking
  # membership alone accepted `nsa,mitre,pss` against an `nsa,mitre` baseline.
  # (Codex raised this on #3057.)
  if [ "$rc" -eq 0 ]; then
    local first_set='' first_wf='' set_i
    for workflow in "${WORKFLOWS[@]}"; do
      set_i="$(framework_set "$workflow")"
      if [ -z "$first_set" ]; then
        first_set="$set_i"
        first_wf="$workflow"
      elif [ "$set_i" != "$first_set" ]; then
        printf '::error::framework sets differ: %s has [%s] but %s has [%s].\n' \
          "$first_wf" "$first_set" "$workflow" "$set_i" >&2
        printf '::error::Both upload to one Code Scanning category, so a difference means findings that never persist. See #2823.\n' >&2
        rc=1
      fi
    done
  fi

  if [ "$rc" -ne 0 ]; then
    exit 1
  fi

  printf 'Kubescape gate: %d required framework(s), identical sets across %d workflow(s).\n' \
    "${#REQUIRED_FRAMEWORKS[@]}" "${#WORKFLOWS[@]}"
}

# Echo a workflow's framework list, normalised (deduplicated, sorted, comma-joined)
# so ordering and repetition cannot make two equal sets compare unequal.
framework_set() {
  scan_invocations "$1" |
    sed -nE 's/.*--framework[[:space:]]+([a-z,]+).*/\1/p' |
    tr ',' '\n' | sed '/^$/d' | sort -u | paste -sd, -
}

check_workflow() {
  local WORKFLOW="$1"

  if [ ! -f "$WORKFLOW" ]; then
    printf '::error::%s not found; nothing was validated\n' "$WORKFLOW" >&2
    return 1
  fi

  # A floor, because an empty result from a filtered read is a claim about the
  # FILTER. If the step is renamed, moved into a composite action, or the flag is
  # spelled differently, this grep returns nothing — and without the floor the
  # guard would exit 0 having checked precisely nothing.
  # `mapfile` is bash 4+; macOS ships bash 3.2, so a contributor on a Mac would
  # get `command not found` (exit 127) while CI's Ubuntu bash passed. Read the
  # matches into the array the portable way instead.
  #
  # COMMENTS ARE EXCLUDED, and that is load-bearing rather than tidiness. This
  # reads raw YAML, so without it a stale comment naming both frameworks would
  # satisfy the guard while the real command ran `--framework "$SOMEVAR"` — the
  # grep would never see the variable form, find only the comment, and exit 0
  # on a repository scanning one framework. A decoy is covered by the test
  # suite. (Codex raised this on #3057.)
  local -a scan_lines=()
  local match
  while IFS= read -r match; do
    [ -n "$match" ] && scan_lines+=("$match")
  done < <(scan_invocations "$WORKFLOW")

  if [ "${#scan_lines[@]}" -eq 0 ]; then
    printf '::error::no executable "ksail workload scan --framework ..." invocation found in %s.\n' "$WORKFLOW" >&2
    printf '::error::The gate moved, was renamed, or the framework list became a variable this guard cannot read.\n' >&2
    printf '::error::Point the guard at it rather than deleting the guard.\n' >&2
    return 1
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

  return "$rc"
}

# Emit `<lineno>:<text>` for each line that INVOKES the scan.
#
# 🔴 THIS IS AN ALLOW-LIST, AND THAT IS THE WHOLE DESIGN. The line's first
# non-blank token must be `ksail`. Anything else — a comment, `echo`, `printf`,
# `cat`, a heredoc body, `:`, `true &&`, a quoted string in a `with:` block — is
# not an invocation and does not count.
#
# The earlier version subtracted known decoys instead: first comments, and then
# `echo` would have been next. That list is unbounded, and each round closes one
# spelling while leaving the class open — so the direction is inverted here.
# Requiring the one shape both workflows actually use closes every spelling at
# once, including the ones nobody has thought of.
#
# Both reviewers on #3057 arrived at this independently, from different decoys.
#
# The cost is deliberate: a legitimate future invocation that is NOT a bare
# `ksail …` line (wrapped in `env`, `xargs`, or a shell function) stops matching
# and trips the fail-closed path. That is the correct direction — the guard
# refuses to bless a form it cannot read, rather than guessing.
scan_invocations() {
  local match text stripped
  while IFS= read -r match; do
    [ -n "$match" ] || continue
    text="${match#*:}"
    stripped="${text#"${text%%[![:space:]]*}"}"
    case "$stripped" in
      ksail[[:space:]]*) printf '%s\n' "$match" ;;
    esac
  done < <(grep -nE 'ksail workload scan[^|]*--framework[[:space:]]+[a-z,]+' "$1" || true)
}

main "$@"
