#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/guard-kubescape-gate-frameworks.sh.
#
# The guard protects against a regression that LOOKS LIKE AN IMPROVEMENT: dropping
# a framework removes findings, so the compliance score rises and every check goes
# green. So the interesting failure mode is the guard passing when it should not —
# every case below therefore has a paired arm that must FAIL, and the wiring half
# asserts the guard is actually reached by CI. A guard nothing calls protects
# nothing (monorepo#2757 is that exact defect in another repo).
#
# The behaviour half runs a COPY of the guard over a synthetic tree. The guard
# derives its root from its own location (`dirname $BASH_SOURCE/..`), so placing
# the copy at <tmp>/scripts/ makes <tmp> the repository it sees. That is the seam;
# without it every case would read the real workflow and nothing could be varied.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard="${root_dir}/scripts/guard-kubescape-gate-frameworks.sh"
readonly workflow="${root_dir}/.github/workflows/ci.yaml"

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}
ok() {
  pass_count=$((pass_count + 1))
  printf 'ok — %s\n' "$1"
}

# Build a synthetic repo whose ci.yaml carries the given scan line, run the guard
# copy against it, and echo its exit status.
readonly GOOD_SCAN='ksail workload scan --framework nsa,mitre --compliance-threshold 95'

# $1 = the ci.yaml scan line under test.
# $2 = optional validate-main.yaml scan line; defaults to a good one so a single
#      failing arm is always attributable to the file it varied.
run_against() {
  local scan_line="$1" main_line="${2:-$GOOD_SCAN}" tmp
  tmp="$(mktemp -d)"
  mkdir -p "${tmp}/scripts" "${tmp}/.github/workflows"
  cp "$guard" "${tmp}/scripts/"
  {
    printf 'jobs:\n  validate:\n    steps:\n      - name: scan\n        run: |\n'
    printf '          %s\n' "$scan_line"
  } >"${tmp}/.github/workflows/ci.yaml"
  {
    printf 'jobs:\n  validate:\n    steps:\n      - name: scan\n        run: |\n'
    printf '          %s\n' "$main_line"
  } >"${tmp}/.github/workflows/validate-main.yaml"
  set +e
  (cd "$tmp" && ./scripts/guard-kubescape-gate-frameworks.sh >/dev/null 2>&1)
  local rc=$?
  set -e
  rm -rf "$tmp"
  printf '%s' "$rc"
}

assert_accepted() {
  local label="$1" line="$2" main="${3:-}" rc
  rc="$(run_against "$line" "${main:-$GOOD_SCAN}")"
  [ "$rc" -eq 0 ] || fail "${label}: expected the guard to ACCEPT (exit 0), got ${rc}"
  ok "accepts ${label}"
}

assert_rejected() {
  local label="$1" line="$2" main="${3:-}" rc
  rc="$(run_against "$line" "${main:-$GOOD_SCAN}")"
  [ "$rc" -ne 0 ] || fail "${label}: expected the guard to REJECT (non-zero), got 0 — it would pass a coverage regression"
  ok "rejects ${label}"
}

# --- GREEN: the shape currently in ci.yaml -----------------------------------
assert_accepted 'the current nsa,mitre configuration' \
  'ksail workload scan --framework nsa,mitre --exceptions /tmp/e.json --compliance-threshold 95 --format sarif -o out.sarif'
assert_accepted 'a different framework ORDER' \
  'ksail workload scan --framework mitre,nsa --exceptions /tmp/e.json --compliance-threshold 95'
# An extra framework is fine ONLY if BOTH workflows carry it — otherwise the two
# analyses disagree under one Code Scanning category. My first version asserted
# the mismatched case as GREEN, which encoded the bug Codex then found.
assert_accepted 'an ADDITIONAL framework present in BOTH workflows' \
  'ksail workload scan --framework nsa,mitre,pss --compliance-threshold 95' \
  'ksail workload scan --framework nsa,mitre,pss --compliance-threshold 95'
assert_rejected 'an ADDITIONAL framework in ci.yaml ONLY (sets differ)' \
  'ksail workload scan --framework nsa,mitre,pss --compliance-threshold 95' \
  'ksail workload scan --framework nsa,mitre --compliance-threshold 95'
assert_rejected 'an ADDITIONAL framework in the MAIN baseline only' \
  'ksail workload scan --framework nsa,mitre --compliance-threshold 95' \
  'ksail workload scan --framework nsa,mitre,pss --compliance-threshold 95'
assert_accepted 'sets equal but written in a different ORDER' \
  'ksail workload scan --framework mitre,nsa,pss --compliance-threshold 95' \
  'ksail workload scan --framework pss,nsa,mitre --compliance-threshold 95'

# --- RED: every way the coverage could silently regress ----------------------
# This is the exact pre-#3057 state, and the regression the guard exists for.
assert_rejected 'the pre-#3057 nsa-only gate' \
  'ksail workload scan --framework nsa --exceptions /tmp/e.json --compliance-threshold 95 --format sarif -o out.sarif'
assert_rejected 'mitre alone' \
  'ksail workload scan --framework mitre --compliance-threshold 95'
assert_rejected 'an unrelated framework replacing both' \
  'ksail workload scan --framework pss --compliance-threshold 95'
# A substring must not satisfy a whole element, or a future rename would pass.
assert_rejected 'a name that merely CONTAINS a required framework' \
  'ksail workload scan --framework nsalike,mitrelike --compliance-threshold 95'

# --- RED: the DECOY — a comment must not satisfy the guard -------------------
# Raised by Codex on #3057. Without comment-stripping the grep sees only the
# comment (the real command's `"$FRAMEWORKS"` never matches `[a-z,]+`), so the
# guard exits 0 while CI evaluates NSA alone. This is the guard's own fail-open.
assert_rejected 'a stale COMMENT naming both frameworks over a variable invocation' \
  '# ksail workload scan --framework nsa,mitre  <- stale comment'
assert_rejected 'an indented comment decoy' \
  '   # ksail workload scan --framework nsa,mitre'
# The allow-list closes the whole class, not just the two spellings reviewers
# happened to send. Each of these would have needed its own exclusion under a
# blacklist; all are rejected by requiring `ksail` as the first token.
assert_rejected 'an echo decoy (output-only, executable)' \
  "echo 'ksail workload scan --framework nsa,mitre'"
assert_rejected 'a printf decoy' \
  "printf '%s' 'ksail workload scan --framework nsa,mitre'"
assert_rejected 'a decoy behind a shell prefix' \
  'true && echo ksail workload scan --framework nsa,mitre'
assert_rejected 'a decoy inside a quoted YAML string value' \
  'FRAMEWORKS_DOC="ksail workload scan --framework nsa,mitre"'

# --- The MAIN-BRANCH BASELINE must match, or alerts never persist ------------
# Both workflows upload under one Code Scanning category, so a baseline scanning
# fewer frameworks means PR-only findings never become durable main alerts — and
# a direct push to main is ungated on the difference. Codex, #3057.
assert_rejected 'a correct ci.yaml with an nsa-only MAIN baseline' \
  "$GOOD_SCAN" \
  'ksail workload scan --framework nsa --compliance-threshold 95'
assert_accepted 'both workflows on nsa,mitre' \
  "$GOOD_SCAN" \
  'ksail workload scan --framework mitre,nsa --compliance-threshold 95'

# --- RED: the guard must fail CLOSED, never pass vacuously -------------------
# If the step is renamed or moved, the grep finds nothing. Exiting 0 there would
# report a protected repository while checking absolutely nothing.
tmp_empty="$(mktemp -d)"
mkdir -p "${tmp_empty}/scripts" "${tmp_empty}/.github/workflows"
cp "$guard" "${tmp_empty}/scripts/"
printf 'jobs:\n  validate:\n    steps:\n      - run: echo no scan here\n' \
  >"${tmp_empty}/.github/workflows/ci.yaml"
printf 'jobs:\n  validate:\n    steps:\n      - run: |\n          %s\n' "$GOOD_SCAN" \
  >"${tmp_empty}/.github/workflows/validate-main.yaml"
set +e
(cd "$tmp_empty" && ./scripts/guard-kubescape-gate-frameworks.sh >/dev/null 2>&1)
rc_empty=$?
set -e
rm -rf "$tmp_empty"
[ "$rc_empty" -ne 0 ] || fail 'no-scan-invocation: expected the guard to FAIL CLOSED, got 0'
ok 'fails closed when no scan invocation is found'

# --- Wiring: the real workflow satisfies the guard, and CI actually calls it --
[ -f "$workflow" ] || fail "wiring: ${workflow} not found"

# A framework name carrying punctuation must survive parsing INTACT. With the
# pre-fix `[a-z,]+` class, `cis-v1.23-t1.0.1` and `cis-v1.24-t1.0.0` both
# truncated to `cis`, so these two workflows — scanning genuinely different
# framework sets — compared EQUAL and the exact-set check reported them
# identical. (Codex raised this on #3057.)
assert_rejected 'framework sets differing only in a punctuated suffix' \
  'ksail workload scan --framework nsa,mitre,cis-v1.23-t1.0.1 --compliance-threshold 95' \
  'ksail workload scan --framework nsa,mitre,cis-v1.24-t1.0.0 --compliance-threshold 95'

assert_accepted 'a punctuated framework name present identically in both' \
  'ksail workload scan --framework nsa,mitre,cis-v1.23-t1.0.1 --compliance-threshold 95' \
  'ksail workload scan --framework nsa,mitre,cis-v1.23-t1.0.1 --compliance-threshold 95'

# A framework list the guard cannot read literally must FAIL CLOSED rather than
# be silently truncated to whatever prefix happens to match.
assert_rejected 'a variable framework list' \
  'ksail workload scan --framework "$FRAMEWORKS" --compliance-threshold 95'

# TWO scan invocations in one workflow must be REJECTED, not unioned: the union
# loses which set produced the SARIF that actually reaches the uploader, so a
# workflow that scans nsa,mitre,pss and then overwrites the same SARIF with an
# nsa,mitre scan compared equal to an nsa,mitre,pss baseline. (Codex, #3057.)
run_two_invocations() {
  local tmp rc
  tmp="$(mktemp -d)"
  mkdir -p "${tmp}/scripts" "${tmp}/.github/workflows"
  cp "$guard" "${tmp}/scripts/"
  {
    printf 'jobs:\n  validate:\n    steps:\n      - name: scan\n        run: |\n'
    printf '          ksail workload scan --framework nsa,mitre,pss --compliance-threshold 95\n'
    printf '          ksail workload scan --framework nsa,mitre --compliance-threshold 95\n'
  } >"${tmp}/.github/workflows/ci.yaml"
  {
    printf 'jobs:\n  validate:\n    steps:\n      - name: scan\n        run: |\n'
    printf '          ksail workload scan --framework nsa,mitre,pss --compliance-threshold 95\n'
  } >"${tmp}/.github/workflows/validate-main.yaml"
  set +e
  (cd "$tmp" && ./scripts/guard-kubescape-gate-frameworks.sh >/dev/null 2>&1)
  rc=$?
  set -e
  rm -rf "$tmp"
  printf '%s' "$rc"
}
[ "$(run_two_invocations)" -ne 0 ] ||
  fail 'two scan invocations: expected REJECT — the union hides which set is uploaded'
ok 'rejects a workflow carrying two scan invocations'

"$guard" >/dev/null || fail 'wiring: the guard does not pass against the real ci.yaml'
ok 'the real ci.yaml satisfies the guard'

# 🔴 THE WIRING ASSERTIONS READ PARSED `run:` VALUES, NEVER RAW FILE TEXT.
#
# A raw-text `grep` for the guard's path is satisfied by any MENTION of it, and
# both workflows contain one: validate-main.yaml carries the comment
# `# scripts/guard-kubescape-gate-frameworks.sh enforces the match.` beside the
# scan step. So deleting the actual `run:` step left this assertion green while a
# direct push to main went unguarded — the exact push-path gap the assertion
# claims to pin. Measured on #3057; Codex raised it.
#
# ⚠️ RESIDUAL, same axis as #3060: this reads the `run:` SCALAR, so a comment
# INSIDE a run block still matches. That is the shell-context class, not the
# YAML-context class closed here, and pretending otherwise is how the previous
# round overclaimed. What is closed is a mention anywhere OUTSIDE an executable
# step, which is where both real decoys live.
workflow_run_values() {
  yq -r '[.jobs[]?.steps[]?.run // ""] | .[]' "$1"
}

command -v yq >/dev/null ||
  fail 'wiring: yq is required to read parsed workflow steps'

# 🔴 CAPTURE FIRST, THEN MATCH — never `yq … | grep -q`. `grep -q` exits on its
# first match and closes the pipe, `yq` dies of SIGPIPE (141), and `pipefail`
# turns the whole pipeline non-zero. The assertion then reports "the guard is not
# wired" on a repository where it IS wired: a false FAILURE that would be
# "fixed" by deleting the assertion. Measured while fixing #3057.
# The paired NEGATIVE arm for the assertions below: a workflow that only MENTIONS
# the guard — in a step name and a YAML comment, with no step running it — must
# not satisfy them. This is the decoy that kept the old raw-text grep green while
# the real `run:` step was deleted, and validate-main.yaml carries exactly such a
# comment today.
decoy_workflow="$(mktemp -d)/mention-only.yaml"
{
  printf '# scripts/guard-kubescape-gate-frameworks.sh enforces the match.\n'
  printf 'jobs:\n  validate:\n    steps:\n'
  printf '      - name: scripts/guard-kubescape-gate-frameworks.sh\n'
  printf '        run: echo unrelated\n'
} >"$decoy_workflow"
grep -qF 'scripts/guard-kubescape-gate-frameworks.sh' "$decoy_workflow" ||
  fail 'decoy: the fixture must contain the mention it is testing'
if grep -qF 'scripts/guard-kubescape-gate-frameworks.sh' <<<"$(workflow_run_values "$decoy_workflow")"; then
  fail 'decoy: a comment/step-name mention satisfied the wiring check — deleting the real step would go unnoticed'
fi
rm -rf "$(dirname "$decoy_workflow")"
ok 'a mention outside an executable step does NOT satisfy the wiring check'

ci_run_values="$(workflow_run_values "$workflow")"
main_run_values="$(workflow_run_values "${root_dir}/.github/workflows/validate-main.yaml")"

grep -qF 'scripts/guard-kubescape-gate-frameworks.sh' <<<"$ci_run_values" ||
  fail 'wiring: no ci.yaml step RUNS the guard — an uncalled guard protects nothing'
ok 'ci.yaml runs the guard in an executable step'

# The guard reads validate-main.yaml, so the real file must satisfy it too — this
# is what would have caught the nsa-only baseline before it shipped.
grep -qE 'ksail workload scan .*--framework[[:space:]]+nsa,mitre' <<<"$main_run_values" ||
  fail 'wiring: validate-main.yaml does not scan nsa,mitre — the main baseline would not match the PR analysis'
ok 'validate-main.yaml scans the same frameworks as the PR gate'

grep -qF 'scripts/guard-kubescape-gate-frameworks.sh' <<<"$main_run_values" ||
  fail 'wiring: no validate-main.yaml step RUNS the guard — a direct push to main would be unchecked'
ok 'validate-main.yaml runs the guard in an executable step'

grep -qF 'scripts/tests/test-kubescape-gate-frameworks-guard.sh' <<<"$ci_run_values" ||
  fail 'wiring: no ci.yaml step RUNS this test — the guard could be widened with every check green'
ok 'ci.yaml runs this test in an executable step'

printf '\nAll %d assertions passed.\n' "$pass_count"
