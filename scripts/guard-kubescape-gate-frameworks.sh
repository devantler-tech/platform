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

# Echo the ONE scan invocation of a workflow as `<lineno>:<text>`.
#
# 🔴 MORE THAN ONE IS REJECTED, NOT MERGED. The earlier version unioned every
# matching line, which loses WHICH set produced the SARIF that actually reaches
# the uploader: a workflow scanning `nsa,mitre,pss` and then overwriting the same
# SARIF with an `nsa,mitre` scan unions to `nsa,mitre,pss` and compares equal to a
# main baseline that only ever analysed three. The uploaded analyses differ while
# the guard reports them identical. (Codex raised this on #3057.)
#
# Both workflows run exactly one scan today, so this is the real shape rather
# than a restriction. A future second invocation trips the fail-closed path and
# has to teach the guard which one feeds the upload.
workflow_invocation() {
  local workflow="$1"
  local -a scan_lines=()
  local match

  while IFS= read -r match; do
    [ -n "$match" ] && scan_lines+=("$match")
  done < <(scan_invocations "$workflow")

  if [ "${#scan_lines[@]}" -eq 0 ]; then
    printf '::error::no executable "ksail workload scan --framework ..." invocation found in %s.\n' "$workflow" >&2
    printf '::error::The gate moved, was renamed, or the framework list became a variable this guard cannot read.\n' >&2
    printf '::error::Point the guard at it rather than deleting the guard.\n' >&2
    return 1
  fi

  if [ "${#scan_lines[@]}" -gt 1 ]; then
    printf '::error::%s has %d scan invocations; the guard cannot tell which one produces the uploaded SARIF.\n' \
      "$workflow" "${#scan_lines[@]}" >&2
    printf '::error::Both workflows upload under one Code Scanning category, so the uploaded set is what must match. See #2823.\n' >&2
    return 1
  fi

  printf '%s\n' "${scan_lines[0]}"
}

# Echo a workflow's framework list, normalised (deduplicated, sorted, comma-joined)
# so ordering and repetition cannot make two equal sets compare unequal.
framework_set() {
  local workflow="$1" invocation argument

  invocation="$(workflow_invocation "$workflow")" || return 1
  argument="$(framework_argument "$invocation")"

  if [ -z "$argument" ]; then
    printf '::error file=%s,line=%s::could not read the --framework value.\n' \
      "$workflow" "${invocation%%:*}" >&2
    return 1
  fi

  framework_tokens "$argument" "${workflow}:${invocation%%:*}" |
    sort -u | paste -sd, -
}

check_workflow() {
  local WORKFLOW="$1"

  if [ ! -f "$WORKFLOW" ]; then
    printf '::error::%s not found; nothing was validated\n' "$WORKFLOW" >&2
    return 1
  fi

  # The floor lives in workflow_invocation: an empty result from a filtered read
  # is a claim about the FILTER, so zero invocations is an error rather than a
  # silent pass, and more than one is rejected instead of merged.
  #
  # COMMENTS ARE EXCLUDED by scan_invocations' allow-list, and that is
  # load-bearing rather than tidiness. This reads raw YAML, so without it a stale
  # comment naming both frameworks would satisfy the guard while the real command
  # ran `--framework "$SOMEVAR"`. A decoy is covered by the test suite.
  # (Codex raised this on #3057.)
  local frameworks
  frameworks="$(framework_set "$WORKFLOW")" || return 1

  if [ -z "$frameworks" ]; then
    printf '::error::%s: the --framework list read as empty.\n' "$WORKFLOW" >&2
    return 1
  fi

  local rc=0 fw
  for fw in "${REQUIRED_FRAMEWORKS[@]}"; do
    # Match a whole comma-separated element, never a substring: `nsa` must not
    # be satisfied by some future `nsa-extended`.
    if ! printf ',%s,' "$frameworks" | grep -qF ",${fw},"; then
      printf '::error file=%s::the Kubescape gate must evaluate "%s", but --framework is "%s".\n' \
        "$WORKFLOW" "$fw" "$frameworks" >&2
      printf '::error::Dropping a framework REMOVES findings, so the compliance score RISES and CI stays green. See #2823.\n' >&2
      rc=1
    fi
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
#
# ⚠️ RESIDUAL, tracked on #3060. This closes the command-SHAPE class but not the
# shell-CONTEXT one: a heredoc BODY line genuinely begins with `ksail`, so it
# still matches while executing nothing. Those are different axes and the
# inversion only covers the first. Closing the second means parsing the `run:`
# scalars with shell awareness, not detecting heredocs here — that is how a
# line-oriented matcher becomes a half-written shell parser. What survives is a
# deliberately-constructed decoy, not a plausible edit; the realistic regression
# is caught.
scan_invocations() {
  local match text stripped
  while IFS= read -r match; do
    [ -n "$match" ] || continue
    text="${match#*:}"
    stripped="${text#"${text%%[![:space:]]*}"}"
    case "$stripped" in
      ksail[[:space:]]*) printf '%s\n' "$match" ;;
    esac
  done < <(grep -nE 'ksail workload scan[^|]*--framework[[:space:]]+[^[:space:]]+' "$1" || true)
}

# Echo the raw `--framework` argument of one `<lineno>:<text>` invocation.
#
# 🔴 THE CHARACTER CLASS IS DELIBERATELY `[^[:space:]]`, NOT `[a-z,]`. A class
# that lists the characters a framework name may contain does not FAIL on an
# unexpected one, it TRUNCATES at it — silently. Measured on #3057: with
# `[a-z,]+`, both `nsa,mitre,cis-v1.23-t1.0.1` and `nsa,mitre,cis-v1.24-t1.0.0`
# normalise to `cis,mitre,nsa`, so two workflows scanning genuinely different
# framework sets compare EQUAL and the exact-set check reports them identical.
# Reading to the next space captures the whole argument, and validation below
# then rejects anything unexpected instead of quietly discarding it.
framework_argument() {
  printf '%s' "${1#*:}" | sed -nE 's/.*--framework[[:space:]]+([^[:space:]]+).*/\1/p'
}

# Echo each comma-separated framework token of one invocation, one per line,
# failing closed on any token that is not a plain framework name.
#
# A `--framework "$FRAMEWORKS"` variable form lands here as `"$FRAMEWORKS"`,
# fails this pattern, and trips the fail-closed path — which is the point. The
# guard refuses to bless a list it cannot read rather than guessing at one.
# Split on commas WITHOUT a pipeline: a `while` at the end of a pipe runs in a
# subshell, where `return 1` unwinds only that subshell and the caller reads a
# success it never earned.
framework_tokens() {
  local argument="$1" location="$2" token rest="$1"

  while [ -n "$rest" ]; do
    token="${rest%%,*}"
    if [ "$token" = "$rest" ]; then
      rest=''
    else
      rest="${rest#*,}"
    fi
    [ -n "$token" ] || continue
    case "$token" in
      *[!a-z0-9._-]*)
        printf '::error::%s: framework token "%s" is not a plain framework name.\n' \
          "$location" "$token" >&2
        printf '::error::The guard reads the literal list; a variable or expression cannot be verified. See #2823.\n' >&2
        return 1
        ;;
    esac
    printf '%s\n' "$token"
  done

  return 0
}

main "$@"
