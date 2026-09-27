#!/usr/bin/env bash
#
# Pins the verdict of guard-deploy-waits-for-merge-group-validators.sh in all three directions (#4148).
#
#   exit 0  deploy-prod needs every merge-group validator ci-required-checks requires
#   exit 1  a required merge-group validator is missing from deploy-prod.needs
#   exit 2  the guard could not check
#
# The first case runs the guard against the REAL committed ci.yaml. Every other case is a small
# fixture workflow that isolates one condition.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-deploy-waits-for-merge-group-validators.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
cases=0

# expect <name> <expected-exit> <workflow-file> [<stderr-substring>]
expect() {
  local name="$1" want="$2" file="$3" needle="${4:-}" got err
  cases=$((cases + 1))
  err="$scratch/stderr"
  bash "$guard" "$file" >/dev/null 2>"$err"
  got=$?
  if [ "$got" -ne "$want" ]; then
    printf 'FAIL %s: exit %s, want %s\n' "$name" "$got" "$want"
    sed 's/^/     /' "$err"
    failures=$((failures + 1))
    return
  fi
  if [ -n "$needle" ] && ! grep -qF -- "$needle" "$err"; then
    printf 'FAIL %s: stderr does not name %s\n' "$name" "$needle"
    sed 's/^/     /' "$err"
    failures=$((failures + 1))
    return
  fi
  printf 'ok   %s\n' "$name"
}

# fixture <name> <deploy-needs> <gate-needs> [<extra-jobs>]
# Declares four validators with the four guard shapes the guard must tell apart.
fixture() {
  local path="$scratch/$1.yaml"
  cat >"$path" <<EOF
on: [pull_request, merge_group]
jobs:
  changes:
    runs-on: ubuntu-latest
  pr-only:
    if: github.event_name == 'pull_request' && needs.changes.outputs.k8s == 'true'
    runs-on: ubuntu-latest
  unguarded:
    runs-on: ubuntu-latest
  either-event:
    if: github.event_name == 'merge_group' || github.event.pull_request.head.repo.fork == false
    runs-on: ubuntu-latest
  pr-or-merge-group:
    if: (github.event_name == 'pull_request' && needs.changes.outputs.k8s == 'true') || github.event_name == 'merge_group'
    runs-on: ubuntu-latest
${4:-}
  deploy-prod:
    needs: $2
    if: github.event_name == 'merge_group'
    runs-on: ubuntu-latest
  ci-required-checks:
    if: always()
    needs: $3
    runs-on: ubuntu-latest
EOF
  printf '%s' "$path"
}

all='[changes, pr-only, unguarded, either-event, pr-or-merge-group, deploy-prod]'

expect "the committed ci.yaml orders the deploy after every merge-group validator" 0 \
  "$repo_root/.github/workflows/ci.yaml"

expect "every merge-group validator needed, pull-request-only one omitted" 0 \
  "$(fixture complete '[changes, unguarded, either-event, pr-or-merge-group]' "$all")"

# Negative controls: drop each merge-group validator in turn. Each shape must be read as running on
# the merge group, or the guard would let that validator run in parallel with the deploy.
expect "a validator with no if: is missing" 1 \
  "$(fixture no-unguarded '[changes, either-event, pr-or-merge-group]' "$all")" "requires unguarded"
expect "a merge_group-or-non-fork validator is missing (the #4148 shape)" 1 \
  "$(fixture no-either '[changes, unguarded, pr-or-merge-group]' "$all")" "requires either-event"
expect "a pull_request-or-merge_group validator is missing" 1 \
  "$(fixture no-mixed '[changes, unguarded, either-event]' "$all")" "requires pr-or-merge-group"

expect "a single-string needs: is read, not skipped" 0 \
  "$(fixture string-needs changes '[changes, pr-only, deploy-prod]')"

# Only the documented leading-conjunct shape is exempt. Any other placement fails closed, so a
# misread costs a false failure rather than an unordered validator.
expect "a pull_request test in another position fails closed" 1 \
  "$(fixture late-pr '[changes, unguarded, either-event, pr-or-merge-group]' \
    '[changes, late-conjunct, unguarded, either-event, pr-or-merge-group, deploy-prod]' \
    "  late-conjunct:
    if: needs.changes.outputs.k8s == 'true' && github.event_name == 'pull_request'
    runs-on: ubuntu-latest")" "requires late-conjunct"

expect "a gate need that names no job cannot be checked" 2 \
  "$(fixture ghost '[changes]' '[changes, ghost-job]')" "ghost-job"

no_gate="$scratch/no-gate.yaml"
printf 'jobs:\n  deploy-prod:\n    needs: [changes]\n' >"$no_gate"
expect "a workflow without ci-required-checks cannot be checked" 2 "$no_gate" "ci-required-checks"

broken="$scratch/broken.yaml"
printf 'jobs:\n  deploy-prod: [unclosed\n' >"$broken"
expect "unparseable YAML cannot be checked" 2 "$broken"

expect "a missing file cannot be checked" 2 "$scratch/does-not-exist.yaml"

printf '\n%s of %s cases passed\n' "$((cases - failures))" "$cases"
[ "$failures" -eq 0 ]
