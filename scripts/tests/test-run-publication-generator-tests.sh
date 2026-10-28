#!/usr/bin/env bash
# The bounded runner must retain every test and every failure in either mode.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
[[ -f "$root/scripts/run-publication-generator-tests.sh" ]] || fail 'the publication test runner is missing'
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fixture="$work/root"
mkdir -p "$fixture/scripts/tests"
cp "$root/scripts/run-publication-generator-tests.sh" "$fixture/scripts/"
tests=(
  test-publish-workflow-approved-revisions.sh
  test-tenant-publication-binding.sh
  test-bootstrap-tenant-source.sh
  test-canonical-source-bootstrap.sh
  test-canonical-source-upgrade.sh
  test-canonical-constructor-attribution.sh
  test-canonical-generator-output.sh
  test-canonical-bootstrap-output.sh
  test-canonical-publisher-regeneration.sh
)
cat >"$work/probe" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
name="$(basename "$0")"
lock() { until mkdir "$PROBE_STATE/lock" 2>/dev/null; do sleep 0.01; done; }
lock
active="$(cat "$PROBE_STATE/active")"
active=$((active+1))
printf '%s\n' "$active" >"$PROBE_STATE/active"
maximum="$(cat "$PROBE_STATE/maximum")"
[[ "$maximum" -ge "$active" ]] || printf '%s\n' "$active" >"$PROBE_STATE/maximum"
printf '%s\n' "$name" >>"$PROBE_STATE/started"
rmdir "$PROBE_STATE/lock"
if [[ ${PROBE_LIMIT:-1} == 2 ]]; then
  for ((attempt=0;attempt<300;attempt++)); do
    [[ "$(cat "$PROBE_STATE/maximum")" -ge 2 ]] && break
    sleep 0.01
  done
  [[ "$(cat "$PROBE_STATE/maximum")" -ge 2 ]] || exit 90
fi
sleep 0.03
printf 'CHILD %s\n' "$name"
lock
active="$(cat "$PROBE_STATE/active")"
printf '%s\n' "$((active-1))" >"$PROBE_STATE/active"
rmdir "$PROBE_STATE/lock"
if [[ ${PROBE_FAILURES:-0} == 1 ]]; then
  case "$name" in
    test-tenant-publication-binding.sh) exit 255 ;;
    test-canonical-publisher-regeneration.sh) exit 7 ;;
  esac
fi
BASH
for test_file in "${tests[@]}"; do cp "$work/probe" "$fixture/scripts/tests/$test_file"; done

run_case() {
  local jobs="$1" failures="$2" label="$3"
  state="$work/$label"
  mkdir "$state"
  printf '0\n' >"$state/active"
  printf '0\n' >"$state/maximum"
  rc=0
  if [[ "$jobs" == default ]]; then
    PROBE_STATE="$state" PROBE_FAILURES="$failures" PROBE_LIMIT=1 \
      bash "$fixture/scripts/run-publication-generator-tests.sh" >"$state/output" 2>&1 || rc=$?
  else
    PROBE_STATE="$state" PROBE_FAILURES="$failures" PROBE_LIMIT="$jobs" \
      bash "$fixture/scripts/run-publication-generator-tests.sh" --jobs "$jobs" >"$state/output" 2>&1 || rc=$?
  fi
}
for jobs in 1 2; do
  run_case "$jobs" 0 "green-$jobs"
  [[ "$rc" == 0 ]] || { cat "$state/output" >&2; fail "jobs=$jobs did not succeed"; }
  [[ "$(wc -l <"$state/started" | tr -d ' ')" == 9 ]] || fail 'a test was skipped or repeated'
  [[ "$(sort -u "$state/started" | wc -l | tr -d ' ')" == 9 ]] || fail 'the full test set was not run'
  [[ "$(cat "$state/maximum")" == "$jobs" ]] || fail "jobs=$jobs did not preserve the concurrency bound"
  grep -q "PUBLICATION_TESTS jobs=$jobs count=9 failed=0" "$state/output" || fail 'the complete success receipt is missing'
  for test_file in "${tests[@]}"; do
    [[ "$(grep -cxF "CHILD $test_file" "$state/output")" == 1 ]] || fail "output lost or repeated for $test_file"
  done
  run_case "$jobs" 1 "failed-$jobs"
  [[ "$rc" == 1 ]] || fail "jobs=$jobs masked a failed child"
  [[ "$(wc -l <"$state/started" | tr -d ' ')" == 9 ]] || fail 'a failure prevented the remaining tests from running'
  grep -q "PUBLICATION_TESTS jobs=$jobs count=9 failed=2" "$state/output" || fail 'not all child failures were retained'
done
run_case default 0 default
[[ "$rc" == 0 && "$(cat "$state/maximum")" == 1 ]] || fail 'the default must retain serial execution'
for jobs in 0 3 text; do
  run_case "$jobs" 0 "invalid-$jobs"
  [[ "$rc" == 2 && ! -e "$state/started" ]] || fail 'an invalid worker limit executed tests'
done
mv "$fixture/scripts/tests/${tests[8]}" "$work/withheld-test"
run_case 2 0 missing
[[ "$rc" == 2 && ! -e "$state/started" ]] || fail 'an incomplete test set was partially executed'
printf 'PASS: both worker limits preserve nine tests, all output, and all failures; invalid and incomplete runs are refused.\n'
