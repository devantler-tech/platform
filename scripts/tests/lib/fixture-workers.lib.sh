#!/usr/bin/env bash
# Test-only workers. Callbacks own distinct case directories and read immutable
# seed data. Drain before a foreground case or any shared-input mutation.

# fixture_pool_error reports a pool contract violation as an infrastructure error.
fixture_pool_error() {
  printf 'fixture-workers: %s\n' "$*" >&2
  return 2
}

# fixture_pool_init creates private tracking files for one or two isolated workers.
fixture_pool_init() {
  case "$2" in 1 | 2) ;; *)
    fixture_pool_error 'workers must be 1 or 2'
    return 2
    ;;
  esac
  FIXTURE_POOL_ROOT="$1/fixture-workers"
  mkdir "$FIXTURE_POOL_ROOT" "$FIXTURE_POOL_ROOT/logs" "$FIXTURE_POOL_ROOT/receipts" || return 2
  FIXTURE_POOL_JOBS=$2 FIXTURE_POOL_PENDING=0 FIXTURE_POOL_FAILED=0 FIXTURE_POOL_INTERRUPTED=0
  FIXTURE_POOL_FINISHED=0 FIXTURE_POOL_VERDICT=0
  : >"$FIXTURE_POOL_ROOT/expected"
  : >"$FIXTURE_POOL_ROOT/pending"
  : >"$FIXTURE_POOL_ROOT/callbacks"
}

# fixture_pool_register admits each declared callback once before cases are submitted.
fixture_pool_register() {
  if ! [[ "$1" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || ! declare -F "$1" >/dev/null; then
    fixture_pool_error 'callback is not a declared function'
    return 2
  fi
  if grep -qxF "$1" "$FIXTURE_POOL_ROOT/callbacks"; then
    fixture_pool_error 'duplicate callback'
    return 2
  fi
  printf '%s\n' "$1" >>"$FIXTURE_POOL_ROOT/callbacks"
}

# fixture_pool_interrupt records cancellation so joining workers cannot report success.
fixture_pool_interrupt() { FIXTURE_POOL_INTERRUPTED=1; }

# fixture_pool_drain joins the current batch and emits every case log before returning failure.
fixture_pool_drain() {
  [ -n "${FIXTURE_POOL_ROOT:-}" ] || return 0
  local pid name result
  while read -r pid name; do
    [ -n "$pid" ] || continue
    while :; do
      result=0
      wait "$pid" || result=$?
      if [ "$result" = 0 ]; then break; fi
      if [ "$result" = 127 ] || ! kill -0 "$pid" 2>/dev/null; then
        FIXTURE_POOL_FAILED=1
        break
      fi
    done
    cat "$FIXTURE_POOL_ROOT/logs/$name" || FIXTURE_POOL_FAILED=1
  done <"$FIXTURE_POOL_ROOT/pending"
  : >"$FIXTURE_POOL_ROOT/pending"
  FIXTURE_POOL_PENDING=0
  [ "$FIXTURE_POOL_INTERRUPTED" = 0 ] || return 2
  [ "$FIXTURE_POOL_FAILED" = 0 ]
}

# fixture_pool_submit assigns a unique case to a registered callback and enforces the worker bound.
fixture_pool_submit() {
  local callback=$1 name=$2 pid
  shift
  [ "$FIXTURE_POOL_FINISHED" = 0 ] && [ "$FIXTURE_POOL_INTERRUPTED" = 0 ] || return 2
  grep -qxF "$callback" "$FIXTURE_POOL_ROOT/callbacks" || {
    fixture_pool_error 'unregistered callback'
    return 2
  }
  [[ "$name" =~ ^[a-zA-Z0-9_-]+$ ]] || {
    fixture_pool_error 'invalid case identifier'
    return 2
  }
  if grep -qxF "$name" "$FIXTURE_POOL_ROOT/expected"; then
    fixture_pool_error 'duplicate case assignment'
    return 2
  fi
  printf '%s\n' "$name" >>"$FIXTURE_POOL_ROOT/expected"
  (
    # The parent owns shared scratch; a worker must never run its EXIT cleanup.
    trap - EXIT HUP INT TERM
    set -euo pipefail
    export FIXTURE_POOL_WORKER=1
    "$callback" "$@"
    printf '%s\n' "$name" >"$FIXTURE_POOL_ROOT/receipts/$name"
  ) >"$FIXTURE_POOL_ROOT/logs/$name" 2>&1 &
  pid=$!
  printf '%s %s\n' "$pid" "$name" >>"$FIXTURE_POOL_ROOT/pending"
  FIXTURE_POOL_PENDING=$((FIXTURE_POOL_PENDING + 1))
  [ "$FIXTURE_POOL_INTERRUPTED" = 0 ] || return 2
  if [ "$FIXTURE_POOL_PENDING" -ge "$FIXTURE_POOL_JOBS" ]; then fixture_pool_drain; fi
}

# fixture_pool_finish joins workers and verifies an exact completion receipt for every assigned case.
# Repeated calls preserve the first complete verdict, including interruption and missing receipts.
fixture_pool_finish() {
  [ -n "${FIXTURE_POOL_ROOT:-}" ] || return 0
  if [ "$FIXTURE_POOL_FINISHED" = 1 ]; then return "$FIXTURE_POOL_VERDICT"; fi
  local result=0 name count receipts=0 file
  fixture_pool_drain || result=$?
  while IFS= read -r name; do
    if ! printf '%s\n' "$name" | cmp -s - "$FIXTURE_POOL_ROOT/receipts/$name"; then result=1; fi
  done <"$FIXTURE_POOL_ROOT/expected"
  count="$(wc -l <"$FIXTURE_POOL_ROOT/expected" | tr -d ' ')"
  for file in "$FIXTURE_POOL_ROOT/receipts/"*; do
    [ -f "$file" ] || continue
    receipts=$((receipts + 1))
  done
  [ "$receipts" = "$count" ] || result=1
  if [ "$FIXTURE_POOL_INTERRUPTED" != 0 ]; then result=2; fi
  FIXTURE_POOL_FINISHED=1 FIXTURE_POOL_VERDICT=$result
  if [ "$result" != 0 ]; then
    printf 'fixture-workers: incomplete or failed cases\n' >&2
    return "$result"
  fi
  printf 'FIXTURE-WORKERS COMPLETE cases=%s completed=%s workers=%s\n' "$count" "$receipts" "$FIXTURE_POOL_JOBS"
}
