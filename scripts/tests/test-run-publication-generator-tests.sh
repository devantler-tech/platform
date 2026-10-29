#!/usr/bin/env bash
# The bounded runner must retain every test and every failure in either mode.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}
[[ -f "$root/scripts/run-publication-generator-tests.sh" ]] || fail 'the publication test runner is missing'
work="$(mktemp -d)"
collector_pid=''
completed=0
cleanup() {
  local result=$?
  trap - EXIT
  if [[ -n "$collector_pid" ]]; then
    kill "$collector_pid" 2>/dev/null || true
    wait "$collector_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
  # Bash 3.2 can mask a nounset abort after an EXIT trap runs.
  [[ "$completed" == 1 || "$result" != 0 ]] || result=1
  exit "$result"
}
trap cleanup EXIT
fixture="$work/root"
mkdir -p "$fixture/scripts/tests"
real_xargs="$(command -v xargs)"
mkdir "$work/bin"
cat >"$work/bin/xargs" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
# Record the actual dispatch bound while retaining the real scheduler.
if [[ $# != 9 || "$1:$2:$3:$4:$5:$6:$8" != "-0:-n:1:-P:$PROBE_LIMIT:bash:--worker" ||
  "$7" != "$PROBE_DRIVER" || ! -d "$9" || -L "$9" ]]; then
  echo 'worker dispatch bound changed' >&2
  exit 95
fi
printf '%s\n' "$5" >"$PROBE_STATE/dispatched-limit"
exec "$PROBE_XARGS" "$@"
BASH
chmod +x "$work/bin/xargs"
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
# Only the collector mutates counters; each child owns a distinct reply FIFO.
exec 4<>"$PROBE_STATE/$name.gate"
printf 'start %s\n' "$name" >&3
IFS= read -r -t 5 release <&4 || exit 90
[[ "$release" == go ]] || exit 91
printf 'CHILD %s\n' "$name"
printf 'end %s\n' "$name" >&3
if [[ ${PROBE_FAILURES:-0} == 1 ]]; then
  case "$name" in
    test-tenant-publication-binding.sh) exit 255 ;;
    test-canonical-publisher-regeneration.sh) exit 7 ;;
  esac
fi
BASH
for test_file in "${tests[@]}"; do cp "$work/probe" "$fixture/scripts/tests/$test_file"; done

release_probe() {
  # A timed-out child may have closed its end already. Read/write mode avoids
  # blocking the observer; the missing end event still fails collection.
  exec 5<>"$state/$1.gate"
  printf 'go\n' >&5
  exec 5>&-
}

collect() {
  local active=0 maximum=0 overlap=0 first='' event name
  while IFS=' ' read -r -t 10 event name <&3; do
    if [[ "$event" == finish && -z "$name" ]]; then
      [[ "$active" == 0 ]] || return 1
      printf '%s\n' "$maximum" >"$state/maximum"
      return 0
    fi
    [[ "$name" =~ ^test-[a-z-]+\.sh$ && -p "$state/$name.gate" ]] || return 1
    case "$event" in
    start)
      [[ ! -e "$state/seen-$name" ]] || return 1
      : >"$state/seen-$name"
      : >"$state/live-$name"
      printf '%s\n' "$name" >>"$state/started"
      active=$((active + 1))
      ((active <= PROBE_LIMIT)) || return 1
      ((maximum >= active)) || maximum=$active
      if [[ "$PROBE_LIMIT" == 2 && "$overlap" == 0 ]]; then
        if [[ "$active" == 1 ]]; then
          first=$name
        else
          # Neither first child can finish before both are observed running.
          release_probe "$first"
          release_probe "$name"
          overlap=1
        fi
      else
        release_probe "$name"
      fi
      ;;
    end)
      [[ -f "$state/live-$name" && "$active" -gt 0 ]] || return 1
      rm "$state/live-$name"
      active=$((active - 1))
      ;;
    *) return 1 ;;
    esac
  done
  return 1
}

run_case() {
  local jobs="$1" failures="$2" label="$3"
  state="$work/$label"
  mkdir "$state"
  mkfifo "$state/events"
  for test_file in "${tests[@]}"; do mkfifo "$state/$test_file.gate"; done
  exec 3<>"$state/events"
  PROBE_LIMIT="$jobs"
  [[ "$jobs" != default ]] || PROBE_LIMIT=1
  collect &
  collector_pid=$!
  rc=0
  if [[ "$jobs" == default ]]; then
    PATH="$work/bin:$PATH" PROBE_XARGS="$real_xargs" PROBE_DRIVER="$fixture/scripts/run-publication-generator-tests.sh" \
      PROBE_STATE="$state" PROBE_FAILURES="$failures" PROBE_LIMIT=1 \
      bash "$fixture/scripts/run-publication-generator-tests.sh" >"$state/output" 2>&1 || rc=$?
  else
    PATH="$work/bin:$PATH" PROBE_XARGS="$real_xargs" PROBE_DRIVER="$fixture/scripts/run-publication-generator-tests.sh" \
      PROBE_STATE="$state" PROBE_FAILURES="$failures" PROBE_LIMIT="$jobs" \
      bash "$fixture/scripts/run-publication-generator-tests.sh" --jobs "$jobs" >"$state/output" 2>&1 || rc=$?
  fi
  printf 'finish\n' >&3
  wait "$collector_pid" || fail 'worker event collection was incomplete or exceeded the bound'
  collector_pid=''
  exec 3>&-
}
for jobs in 1 2; do
  run_case "$jobs" 0 "green-$jobs"
  [[ "$rc" == 0 ]] || {
    cat "$state/output" >&2
    fail "jobs=$jobs did not succeed"
  }
  [[ -f "$state/dispatched-limit" && "$(cat "$state/dispatched-limit")" == "$jobs" ]] ||
    fail 'the actual dispatcher limit receipt is missing or wrong'
  [[ "$(wc -l <"$state/started" | tr -d ' ')" == 9 ]] || fail 'a test was skipped or repeated'
  [[ "$(sort -u "$state/started" | wc -l | tr -d ' ')" == 9 ]] || fail 'the full test set was not run'
  [[ "$(cat "$state/maximum")" == "$jobs" ]] || fail "jobs=$jobs did not preserve the concurrency bound"
  grep -q "PUBLICATION_TESTS jobs=$jobs count=9 failed=0" "$state/output" || fail 'the complete success receipt is missing'
  for test_file in "${tests[@]}"; do
    [[ "$(grep -cxF "CHILD $test_file" "$state/output")" == 1 ]] || fail "output lost or repeated for $test_file"
  done
  run_case "$jobs" 1 "failed-$jobs"
  [[ "$rc" == 1 ]] || fail "jobs=$jobs masked a failed child"
  [[ -f "$state/dispatched-limit" && "$(cat "$state/dispatched-limit")" == "$jobs" ]] ||
    fail 'the failed run did not retain its actual dispatcher limit'
  [[ "$(wc -l <"$state/started" | tr -d ' ')" == 9 ]] || fail 'a failure prevented the remaining tests from running'
  grep -q "PUBLICATION_TESTS jobs=$jobs count=9 failed=2" "$state/output" || fail 'not all child failures were retained'
done
run_case default 0 default
[[ "$rc" == 0 && "$(cat "$state/maximum")" == 1 ]] || fail 'the default must retain serial execution'
[[ -f "$state/dispatched-limit" && "$(cat "$state/dispatched-limit")" == 1 ]] ||
  fail 'the default dispatcher did not use one worker'
for jobs in 0 3 text; do
  run_case "$jobs" 0 "invalid-$jobs"
  [[ "$rc" == 2 && ! -e "$state/started" && ! -e "$state/dispatched-limit" ]] ||
    fail 'an invalid worker limit dispatched tests'
done
mv "$fixture/scripts/tests/${tests[8]}" "$work/withheld-test"
run_case 2 0 missing
[[ "$rc" == 2 && ! -e "$state/started" && ! -e "$state/dispatched-limit" ]] ||
  fail 'an incomplete test set was dispatched'
completed=1
printf 'PASS: both worker limits preserve nine tests, all output, and all failures; invalid and incomplete runs are refused.\n'
