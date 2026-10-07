#!/usr/bin/env bash
# Exercise actual scheduler cancellation and fixture-building failure paths.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
parent=''
cleanup() {
  local file pid
  if [ -n "$parent" ]; then
    kill -TERM "$parent" 2>/dev/null || true
  fi
  # These PID files are written only by this test's own synthetic processes.
  for file in "$scratch"/*/guard.pid "$scratch"/*/child.pid; do
    [ -f "$file" ] || continue
    pid="$(cat "$file")"
    case "$pid" in '' | *[!0-9]*) continue ;; esac
    kill -TERM "$pid" 2>/dev/null || true
  done
  if [ -n "$parent" ]; then
    wait "$parent" 2>/dev/null || true
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT

wait_for_file() {
  local file="$1" _
  for _ in $(seq 1 150); do
    [ ! -f "$file" ] || return 0
    sleep 0.02
  done
  printf 'timed out waiting for %s\n' "$file" >&2
  return 1
}

wait_for_witness_start() {
  local file="$1" _
  # Preparation uses real fixture tools and has its own bounded readiness wait.
  # Cancellation is measured only after the witness starts; its bounds below
  # remain unchanged. A failed producer must never look like a ready witness.
  for _ in $(seq 1 750); do
    if ! kill -0 "$parent" 2>/dev/null; then
      printf 'fixture producer exited before witness readiness\n' >&2
      return 1
    fi
    [ ! -f "$file" ] || return 0
    sleep 0.02
  done
  printf 'timed out waiting for fixture witness readiness: %s\n' "$file" >&2
  return 1
}

wait_for_parent_exit() {
  local _
  for _ in $(seq 1 150); do
    if ! kill -0 "$parent" 2>/dev/null; then return 0; fi
    sleep 0.02
  done
  printf 'scheduler did not stop its synchronous worker\n' >&2
  return 1
}

assert_stopped() {
  local state="$1" role pid _
  for role in guard child; do
    pid="$(cat "$state/$role.pid")"
    for _ in $(seq 1 150); do
      if ! kill -0 "$pid" 2>/dev/null; then break; fi
      sleep 0.02
    done
    if kill -0 "$pid" 2>/dev/null; then
      printf '%s process outlived scheduler cleanup\n' "$role" >&2
      return 1
    fi
  done
  grep -qx 'present' "$state/fixture-at-stop" || {
    printf 'fixture was removed before its worker stopped\n' >&2
    return 1
  }
}

mkdir -p "$scratch/fake-repo/scripts/tests" "$scratch/bin"
cp "$repo_root/scripts/tests/run-parallel-guard-cases.sh" "$scratch/fake-repo/scripts/tests/"
cp "$repo_root/scripts/tests/test-guard-consumer-discovery-conservation.sh" "$scratch/fake-repo/scripts/tests/"
cat >"$scratch/fake-repo/scripts/tests/test-run-parallel-guard-cases.sh" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

fake_guard="$scratch/fake-repo/scripts/guard-consumer-discovery-conservation.sh"
cat >"$fake_guard" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${PARALLEL_CLEANUP_STATE:?}"
root="$1"
stop() {
  if [ -d "$root" ]; then printf 'present\n'; else printf 'missing\n'; fi >"$state/fixture-at-stop"
  exit 143
}
trap stop TERM INT
printf '%s\n' "$$" >"$state/guard.pid"
sleep 60 &
child=$!
printf '%s\n' "$child" >"$state/child.pid"
touch "$state/started"
if [ -n "${CLOSURE_RENDER_ATTEMPT:-}${REMOTE_RENDER_ATTEMPT:-}" ]; then
  touch "$state/witness-started"
fi
wait "$child"
EOF
chmod +x "$fake_guard" "$scratch/fake-repo/scripts/tests/run-parallel-guard-cases.sh"

# Stop the standalone scheduler while a guard and its child are running.
state="$scratch/interrupted"
mkdir -p "$state" "$scratch/cases/01" "$scratch/tree"
printf 'pass\n' >"$scratch/cases/01/kind"
printf 'interrupted worker\n' >"$scratch/cases/01/label"
printf '%s\n' "$scratch/tree" >"$scratch/cases/01/root"
printf 'expected\0' >"$scratch/cases/01/wants"
PARALLEL_CLEANUP_STATE="$state" bash "$repo_root/scripts/tests/run-parallel-guard-cases.sh" \
  "$fake_guard" "$scratch/cases" 1 >"$state/output" 2>&1 &
parent=$!
wait_for_file "$state/started"
kill -TERM "$parent"
rc=0
wait "$parent" || rc=$?
parent=''
[ "$rc" -eq 143 ] || { printf 'cancellation lost its failing exit code: %s\n' "$rc" >&2; exit 1; }
assert_stopped "$state"

# Fail actual fixture construction immediately after the first queued worker
# starts. The real suite must stop/join it before deleting its fixture tree.
cat >"$scratch/bin/cp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${PARALLEL_CLEANUP_STATE:?}"
case "${*: -1}" in
  */cases/0002/tree)
    for _ in $(seq 1 150); do
      [ ! -f "$state/started" ] || exit 42
      sleep 0.02
    done
    exit 43
    ;;
esac
exec "${PARALLEL_CLEANUP_REAL_CP:?}" "$@"
EOF
chmod +x "$scratch/bin/cp"
state="$scratch/fixture-failure"
mkdir -p "$state"
real_cp="$(command -v cp)"
rc=0
PATH="$scratch/bin:$PATH" PARALLEL_CLEANUP_REAL_CP="$real_cp" PARALLEL_CLEANUP_STATE="$state" \
  bash "$scratch/fake-repo/scripts/tests/test-guard-consumer-discovery-conservation.sh" \
  >"$state/output" 2>&1 || rc=$?
[ "$rc" -eq 42 ] || {
  printf 'fixture failure lost its exit code: %s\n' "$rc" >&2
  cat "$state/output" >&2
  exit 1
}
assert_stopped "$state"

# Marker witnesses still wait synchronously, but must also be cancellable while
# their guard blocks. Use the actual selected inline-document family.
state="$scratch/interrupted-witness"
mkdir -p "$state" "$scratch/witness-bin"
# Fixture setup precedes the cancellation measurement. Exercise actual slow
# preparation without giving the interrupted worker more time to stop.
real_yq="$(command -v yq)"
cat >"$scratch/witness-bin/yq" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${PARALLEL_CLEANUP_STATE:?}"
if mkdir "$state/setup-delayed" 2>/dev/null; then sleep 5; fi
exec "${PARALLEL_CLEANUP_REAL_YQ:?}" "$@"
EOF
chmod +x "$scratch/witness-bin/yq"
PATH="$scratch/witness-bin:$PATH" PARALLEL_CLEANUP_REAL_YQ="$real_yq" \
  PARALLEL_CLEANUP_STATE="$state" CONSUMER_CONSERVATION_REGRESSION=inline-documents \
  bash "$scratch/fake-repo/scripts/tests/test-guard-consumer-discovery-conservation.sh" \
  >"$state/output" 2>&1 &
parent=$!
wait_for_witness_start "$state/witness-started"
kill -TERM "$parent"
wait_for_parent_exit
rc=0
wait "$parent" || rc=$?
parent=''
[ "$rc" -eq 143 ] || { printf 'witness cancellation lost its exit code: %s\n' "$rc" >&2; exit 1; }
assert_stopped "$state"

# A producer that exits before readiness must fail, rather than consuming the
# setup deadline or allowing the cancellation assertions to inspect nothing.
state="$scratch/exited-before-witness"
mkdir -p "$state"
(exit 42) &
parent=$!
if wait_for_witness_start "$state/witness-started" >"$state/output" 2>&1; then
  printf 'an exited fixture producer was accepted as ready\n' >&2
  exit 1
fi
rc=0
wait "$parent" || rc=$?
parent=''
[ "$rc" -eq 42 ] || { printf 'fixture producer lost its failing exit code\n' >&2; exit 1; }
grep -qF 'fixture producer exited before witness readiness' "$state/output" || {
  printf 'fixture exit was mistaken for a readiness timeout\n' >&2
  cat "$state/output" >&2
  exit 1
}

# Selected regression families must join queued cases before choosing a verdict.
# A high admission bound ensures no scheduling wait can accidentally hide a
# missing final drain. Every synthetic guard result deliberately fails its case.
cat >"$fake_guard" <<'EOF'
#!/usr/bin/env bash
printf 'deliberately wrong output\n'
exit 1
EOF
state="$scratch/selected-family"
mkdir -p "$state"
rc=0
CONSUMER_CONSERVATION_REGRESSION=identity CONSUMER_DISCOVERY_TEST_JOBS=10000 \
  bash "$scratch/fake-repo/scripts/tests/test-guard-consumer-discovery-conservation.sh" \
  >"$state/output" 2>&1 || rc=$?
if [ "$rc" -eq 0 ]; then
  printf 'selected regression family ignored failing queued workers\n' >&2
  exit 1
fi
grep -qF 'refused for the wrong reason' "$state/output" || {
  printf 'selected regression family failed without checking worker verdicts\n' >&2
  cat "$state/output" >&2
  exit 1
}

printf 'parallel guard cleanup contract passed\n'
