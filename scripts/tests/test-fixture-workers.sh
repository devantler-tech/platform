#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cat >"$work/driver" <<'DRIVER'
#!/usr/bin/env bash
set -euo pipefail
source "$POOL_LIBRARY"
fixture_pool_init "$FIXTURE" "${JOBS:-2}"
# cleanup_driver joins the synthetic pool before its temporary test files can be removed.
cleanup_driver() {
  local result=$?
  trap - EXIT
  fixture_pool_finish >/dev/null || true
  exit "$result"
}
trap cleanup_driver EXIT
trap fixture_pool_interrupt HUP INT TERM
# work_case supplies controlled success, early failure, corruption and interruption scenarios.
work_case() {
  local name=$1
  mkdir "$FIXTURE/active-$name"
  [ "$(find "$FIXTURE" -maxdepth 1 -type d -name 'active-*' | wc -l | tr -d ' ')" -le "$FIXTURE_POOL_JOBS" ]
  [ "$name" != failed ] || return 17
  if [ "$name" = early-error ]; then false; printf 'unsafe\n' >"$FIXTURE/after-error"; fi
  if [ "$MODE" = good ] && [ "$FIXTURE_POOL_JOBS" = 2 ]; then
    # The first worker must observe its peer before either completes. This
    # proves overlap rather than relying on a timing-only speed assertion.
    local started=$SECONDS
    until [ "$(find "$FIXTURE" -maxdepth 1 -type d -name 'active-*' | wc -l | tr -d ' ')" = 2 ]; do
      [ "$((SECONDS - started))" -lt 5 ] || return 19
      sleep 0.02
    done
  fi
  sleep 0.15
  [ -d "$FIXTURE" ]
  printf '%s\n' "$name" >"$FIXTURE/result-$name"
  rmdir "$FIXTURE/active-$name"
}
fixture_pool_register work_case
case "$MODE" in
good)
  for name in alpha beta gamma delta; do fixture_pool_submit work_case "$name"; done
  ;;
failed | early-error) fixture_pool_submit work_case "$MODE" ;;
peer-failure)
  fixture_pool_submit work_case failed
  fixture_pool_submit work_case alpha
  ;;
missing | corrupt | extra)
  fixture_pool_submit work_case alpha
  fixture_pool_drain
  if [ "$MODE" = missing ]; then rm "$FIXTURE/fixture-workers/receipts/alpha";
  elif [ "$MODE" = corrupt ]; then printf 'other\n' >"$FIXTURE/fixture-workers/receipts/alpha";
  else printf 'unassigned\n' >"$FIXTURE/fixture-workers/receipts/unassigned"; fi
  ;;
duplicate)
  fixture_pool_submit work_case alpha
  fixture_pool_submit work_case alpha
  ;;
unregistered) fixture_pool_submit unknown alpha ;;
interrupted)
  fixture_pool_submit work_case alpha
  fixture_pool_interrupt
  ;;
signalled)
  fixture_pool_submit work_case alpha
  kill -TERM "$$"
  ;;
esac
fixture_pool_finish
DRIVER
chmod +x "$work/driver"
library="$root/scripts/tests/lib/fixture-workers.lib.sh"
for mode in good failed early-error peer-failure missing corrupt extra duplicate unregistered interrupted signalled; do
  mkdir "$work/$mode"
  result=0
  POOL_LIBRARY="$library" FIXTURE="$work/$mode" MODE="$mode" \
    "$BASH" "$work/driver" >"$work/$mode.log" 2>&1 || result=$?
  if [ "$mode" = good ]; then
    if [ "$result" != 0 ] || ! grep -q 'FIXTURE-WORKERS COMPLETE cases=4 completed=4 workers=2' "$work/$mode.log"; then
      cat "$work/$mode.log" >&2
      exit 1
    fi
    for name in alpha beta gamma delta; do [ "$(cat "$work/$mode/result-$name")" = "$name" ]; done
  elif [ "$result" = 0 ]; then
    printf 'FAIL: %s was accepted\n' "$mode" >&2
    exit 1
  else
    case "$mode" in
      duplicate) reason='duplicate case assignment' ;;
      unregistered) reason='unregistered callback' ;;
      *) reason='incomplete or failed cases' ;;
    esac
    grep -q "$reason" "$work/$mode.log" || {
      cat "$work/$mode.log" >&2
      exit 1
    }
    if grep -q 'FIXTURE-WORKERS COMPLETE' "$work/$mode.log"; then
      echo 'FAIL: unsuccessful work emitted a completion verdict' >&2
      exit 1
    fi
  fi
  case "$mode" in
    interrupted | signalled | peer-failure) [ "$(cat "$work/$mode/result-alpha")" = alpha ] ;;
  esac
  [ ! -e "$work/$mode/after-error" ] || {
    echo 'FAIL: worker ignored early failure' >&2
    exit 1
  }
done
mkdir "$work/serial"
POOL_LIBRARY="$library" FIXTURE="$work/serial" MODE=good JOBS=1 \
  "$BASH" "$work/driver" >"$work/serial.log" 2>&1
grep -q 'FIXTURE-WORKERS COMPLETE cases=4 completed=4 workers=1' "$work/serial.log"
for jobs in 0 3 unknown; do
  mkdir "$work/jobs-$jobs"
  if POOL_LIBRARY="$library" FIXTURE="$work/jobs-$jobs" MODE=good JOBS="$jobs" \
    "$BASH" "$work/driver" >"$work/jobs-$jobs.log" 2>&1; then
    echo 'FAIL: invalid worker bound accepted' >&2
    exit 1
  fi
  grep -q 'workers must be 1 or 2' "$work/jobs-$jobs.log"
done
# Exercise the real caller cleanup without running the long rollout suite.
# A parent failure must retain its status and pending-worker diagnostics.
sed -n '/^cleanup() {$/,/^}$/p' "$root/scripts/tests/test-data-product-ui-rollout.sh" >"$work/cleanup-body"
[ "$(grep -c '^cleanup() {' "$work/cleanup-body")" = 1 ]
cat >"$work/cleanup-driver" <<'CLEANUP'
#!/usr/bin/env bash
set -euo pipefail
# fixture_pool_finish models a pending worker diagnostic while requiring scratch to remain present.
fixture_pool_finish() {
  [ -d "$scratch" ] || return 98
  printf 'pending-worker diagnostic\n'
  return "$WORKER_RESULT"
}
CLEANUP
cat "$work/cleanup-body" >>"$work/cleanup-driver"
cat >>"$work/cleanup-driver" <<'CLEANUP'
trap cleanup EXIT
exit "$PARENT_RESULT"
CLEANUP
for parent_result in 0 11; do
  for worker_result in 0 7; do
    scratch="$work/cleanup-$parent_result-$worker_result"
    mkdir "$scratch"
    result=0 expected=$parent_result
    if [ "$expected" = 0 ]; then expected=$worker_result; fi
    scratch="$scratch" PARENT_RESULT="$parent_result" WORKER_RESULT="$worker_result" \
      "$BASH" "$work/cleanup-driver" >"$work/cleanup-out" 2>"$work/cleanup-err" || result=$?
    if [[ "$result" != "$expected" || -e "$scratch" || -s "$work/cleanup-out" ]]; then
      echo 'FAIL: caller cleanup lost exit status or shared scratch ordering' >&2
      exit 1
    fi
    grep -qxF 'pending-worker diagnostic' "$work/cleanup-err" || {
      echo 'FAIL: caller cleanup discarded pending-worker diagnostics' >&2
      exit 1
    }
  done
done
printf 'PASS: complete isolated workers; failures, early errors, missing/corrupt receipts, duplicate/unknown cases, interruption and invalid bounds refused\n'
