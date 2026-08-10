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

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
ok() { pass_count=$((pass_count + 1)); printf 'ok — %s\n' "$1"; }

# Build a synthetic repo whose ci.yaml carries the given scan line, run the guard
# copy against it, and echo its exit status.
run_against() {
  local scan_line="$1" tmp
  tmp="$(mktemp -d)"
  mkdir -p "${tmp}/scripts" "${tmp}/.github/workflows"
  cp "$guard" "${tmp}/scripts/"
  {
    printf 'jobs:\n  validate:\n    steps:\n      - name: scan\n        run: |\n'
    printf '          %s\n' "$scan_line"
  } > "${tmp}/.github/workflows/ci.yaml"
  set +e
  ( cd "$tmp" && ./scripts/guard-kubescape-gate-frameworks.sh >/dev/null 2>&1 )
  local rc=$?
  set -e
  rm -rf "$tmp"
  printf '%s' "$rc"
}

assert_accepted() {
  local label="$1" line="$2" rc
  rc="$(run_against "$line")"
  [ "$rc" -eq 0 ] || fail "${label}: expected the guard to ACCEPT (exit 0), got ${rc}"
  ok "accepts ${label}"
}

assert_rejected() {
  local label="$1" line="$2" rc
  rc="$(run_against "$line")"
  [ "$rc" -ne 0 ] || fail "${label}: expected the guard to REJECT (non-zero), got 0 — it would pass a coverage regression"
  ok "rejects ${label}"
}

# --- GREEN: the shape currently in ci.yaml -----------------------------------
assert_accepted 'the current nsa,mitre configuration' \
  'ksail workload scan --framework nsa,mitre --exceptions /tmp/e.json --compliance-threshold 95 --format sarif -o out.sarif'
assert_accepted 'a different framework ORDER' \
  'ksail workload scan --framework mitre,nsa --exceptions /tmp/e.json --compliance-threshold 95'
assert_accepted 'an ADDITIONAL framework alongside the required ones' \
  'ksail workload scan --framework nsa,mitre,pss --compliance-threshold 95'

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

# --- RED: the guard must fail CLOSED, never pass vacuously -------------------
# If the step is renamed or moved, the grep finds nothing. Exiting 0 there would
# report a protected repository while checking absolutely nothing.
tmp_empty="$(mktemp -d)"
mkdir -p "${tmp_empty}/scripts" "${tmp_empty}/.github/workflows"
cp "$guard" "${tmp_empty}/scripts/"
printf 'jobs:\n  validate:\n    steps:\n      - run: echo no scan here\n' \
  > "${tmp_empty}/.github/workflows/ci.yaml"
set +e
( cd "$tmp_empty" && ./scripts/guard-kubescape-gate-frameworks.sh >/dev/null 2>&1 )
rc_empty=$?
set -e
rm -rf "$tmp_empty"
[ "$rc_empty" -ne 0 ] || fail 'no-scan-invocation: expected the guard to FAIL CLOSED, got 0'
ok 'fails closed when no scan invocation is found'

# --- Wiring: the real workflow satisfies the guard, and CI actually calls it --
[ -f "$workflow" ] || fail "wiring: ${workflow} not found"

"$guard" >/dev/null || fail 'wiring: the guard does not pass against the real ci.yaml'
ok 'the real ci.yaml satisfies the guard'

grep -qF 'scripts/guard-kubescape-gate-frameworks.sh' "$workflow" \
  || fail 'wiring: ci.yaml never invokes the guard — an uncalled guard protects nothing'
ok 'ci.yaml invokes the guard'

grep -qF 'scripts/tests/test-kubescape-gate-frameworks-guard.sh' "$workflow" \
  || fail 'wiring: ci.yaml never runs THIS test — the guard could be widened with every check green'
ok 'ci.yaml runs this test'

printf '\nAll %d assertions passed.\n' "$pass_count"
