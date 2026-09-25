#!/usr/bin/env bash
#
# Pins scripts/merge-group-evicted.sh against a stub `gh` command that answers each read
# with the next line of a scripted sequence of queue states.
#
# A stub line is "<PR state> <isInMergeQueue> <entry enqueuedAt|none> <queued entries>".

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/merge-group-evicted.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

[ -x "$script" ] || { printf 'FAIL: %s is not executable\n' "$script"; exit 1; }

sha="$(printf 'c%.0s' {1..40})"
queue_ref="refs/heads/gh-readonly-queue/main/pr-3084-$sha"
created="2026-09-25T10:00:00Z"
before="2026-09-25T09:59:00Z"
after="2026-09-25T10:05:00Z"

mkdir -p "$scratch/bin"
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
[ "$1 $2" = "api graphql" ] || exit 99
[ "${STUB_RC:-0}" = 0 ] || exit "$STUB_RC"
calls=$(($(cat "$STUB_CALLS") + 1))
printf '%s\n' "$calls" >"$STUB_CALLS"
line="$(sed -n "${calls}p" "$STUB_STATES")"
# Past the end of the script, keep repeating the last state.
[ -n "$line" ] || line="$(tail -n 1 "$STUB_STATES")"
printf '%s\n' "$line"
EOF
chmod +x "$scratch/bin/gh"

# <label> <expected-rc> <expected-output> <expected-reads|-> <head ref> <created at> <stub rc> <state>...
run_case() {
  local label="$1" want_rc="$2" want_output="$3" want_reads="$4" ref="$5" at="$6" stub_rc="$7"
  shift 7
  local out rc got_output reads
  : >"$scratch/log"
  : >"$scratch/output"
  printf '0\n' >"$scratch/calls"
  printf '%s\n' "$@" >"$scratch/states"
  if out="$(PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_STATES="$scratch/states" \
    STUB_CALLS="$scratch/calls" STUB_RC="$stub_rc" EVICTED_POLL_SECONDS=0 EVICTED_MAX_POLLS=5 \
    EVICTED_REPOSITORY=devantler-tech/platform EVICTED_HEAD_REF="$ref" EVICTED_GROUP_CREATED_AT="$at" \
    GITHUB_OUTPUT="$scratch/output" "$script" 2>&1)"; then rc=0; else rc=$?; fi
  got_output="$(cat "$scratch/output")"
  reads="$(cat "$scratch/calls")"

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [ "$got_output" = "$want_output" ] &&
    { [ "$want_reads" = - ] || [ "$reads" = "$want_reads" ]; }; then
    printf '  ok   %s (exit %s, output=%s, reads=%s)\n' "$label" "$rc" "${got_output:-<none>}" "$reads"
  else
    printf '  FAIL %s: expected exit %s, output=%s, reads=%s; got exit %s, output=%s, reads=%s:\n%s\n' \
      "$label" "$want_rc" "${want_output:-<none>}" "$want_reads" "$rc" "${got_output:-<none>}" "$reads" "$out"
    failures=$((failures + 1))
  fi
}

# <label> <fixed string the gh call log must contain>
logged() {
  assertions=$((assertions + 1))
  if grep -Fq -- "$2" "$scratch/log"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: gh was not called with %s:\n%s\n' "$1" "$2" "$(cat "$scratch/log")"
    failures=$((failures + 1))
  fi
}

echo "merge-group-evicted:"
run_case "already merged is not evicted" 0 "evicted=false" 1 "$queue_ref" "$created" 0 \
  "MERGED false none 0"
logged "reads the PR number from the ref" "-F number=3084"
logged "reads the queue of the ref's base branch" "-f base=main"

# Finding: a single read right after the deploy is a snapshot. The PR can still time out
# before the required checks settle, so the original entry is polled to a terminal state.
run_case "still queued is polled until it merges" 0 "evicted=false" 3 "$queue_ref" "$created" 0 \
  "OPEN true $before 1" "OPEN true $before 1" "MERGED false none 0"
run_case "still queued is polled until it leaves" 0 "evicted=true" 2 "$queue_ref" "$created" 0 \
  "OPEN true $before 1" "OPEN false none 0"
run_case "an entry enqueued at the group's creation is the original" 0 "evicted=false" 2 "$queue_ref" "$created" 0 \
  "OPEN true $created 1" "MERGED false none 0"
run_case "still queued past the bound writes nothing" 1 "" 5 "$queue_ref" "$created" 0 \
  "OPEN true $before 1"

# Finding: after a dequeue and re-enqueue, isInMergeQueue is true for the REPLACEMENT entry.
# This group has left; the replacement is a queued group that will deploy over it.
run_case "a re-enqueued PR defers to its replacement group" 0 "evicted=false" 1 "$queue_ref" "$created" 0 \
  "OPEN true $after 1"

# Finding: a later group's deploy can precede the heal in the prod-deploy queue, and
# restoring main would publish main without that group's change over its deployment.
run_case "left with another group queued defers to it" 0 "evicted=false" 1 "$queue_ref" "$created" 0 \
  "OPEN false none 2"
run_case "left with nothing queued is evicted" 0 "evicted=true" 1 "$queue_ref" "$created" 0 \
  "OPEN false none 0"
run_case "closed with nothing queued is evicted" 0 "evicted=true" 1 "$queue_ref" "$created" 0 \
  "CLOSED false none 0"

run_case "failed read writes nothing" 1 "" 0 "$queue_ref" "$created" 1 "OPEN false none 0"
run_case "missing PR writes nothing" 1 "" 1 "$queue_ref" "$created" 0 "null null none none"
run_case "missing queue count writes nothing" 1 "" 1 "$queue_ref" "$created" 0 "OPEN false none none"
run_case "unknown state writes nothing" 1 "" 1 "$queue_ref" "$created" 0 "OPEN maybe none 0"
run_case "queued without an entry time writes nothing" 1 "" 1 "$queue_ref" "$created" 0 "OPEN true none 1"
run_case "non-queue ref writes nothing" 1 "" 0 "refs/heads/main" "$created" 0 "OPEN false none 0"
run_case "short head sha is refused" 1 "" 0 "refs/heads/gh-readonly-queue/main/pr-3084-abc123" "$created" 0 \
  "OPEN false none 0"
run_case "PR number zero is refused" 1 "" 0 "refs/heads/gh-readonly-queue/main/pr-0-$sha" "$created" 0 \
  "OPEN false none 0"
run_case "non-UTC group time is refused" 1 "" 0 "$queue_ref" "2026-09-25T12:00:00+02:00" 0 "OPEN false none 0"
run_case "+00:00 group time is read as UTC" 0 "evicted=false" 1 "$queue_ref" "2026-09-25T10:00:00+00:00" 0 \
  "OPEN true $after 1"
run_case "base branch with a slash" 0 "evicted=true" 1 "refs/heads/gh-readonly-queue/release/v1/pr-42-$sha" \
  "$created" 0 "OPEN false none 0"
logged "reads the queue of a slashed base branch" "-f base=release/v1"

echo "merge-group-evicted: $assertions assertions, $failures failed"
[ "$failures" = 0 ]
