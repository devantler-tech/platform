#!/usr/bin/env bash
#
# Pins scripts/merge-group-evicted.sh against a stub `gh` command.

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

mkdir -p "$scratch/bin"
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
[ "$1 $2" = "api graphql" ] || exit 99
[ "${STUB_RC:-0}" = 0 ] || exit "$STUB_RC"
printf '%s\n' "$STUB_STATE"
EOF
chmod +x "$scratch/bin/gh"

# <label> <expected-rc> <expected-output-file> <head ref> <stub state> <stub rc> [expected pr number]
run_case() {
  local label="$1" want_rc="$2" want_output="$3" out rc got_output number_ok=yes
  : >"$scratch/log"
  : >"$scratch/output"
  if out="$(PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_STATE="$5" STUB_RC="$6" \
    EVICTED_REPOSITORY=devantler-tech/platform EVICTED_HEAD_REF="$4" GITHUB_OUTPUT="$scratch/output" \
    "$script" 2>&1)"; then rc=0; else rc=$?; fi
  got_output="$(cat "$scratch/output")"
  if [ -n "${7:-}" ]; then
    grep -Fq -- "-F number=$7" "$scratch/log" || number_ok=no
  fi

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [ "$got_output" = "$want_output" ] && [ "$number_ok" = yes ]; then
    printf '  ok   %s (exit %s, output=%s)\n' "$label" "$rc" "${got_output:-<none>}"
  else
    printf '  FAIL %s: expected exit %s, output=%s; got exit %s, output=%s, number_ok=%s:\n%s\n' \
      "$label" "$want_rc" "${want_output:-<none>}" "$rc" "${got_output:-<none>}" "$number_ok" "$out"
    failures=$((failures + 1))
  fi
}

echo "merge-group-evicted:"
run_case "still queued is not evicted" 0 "evicted=false" "$queue_ref" "OPEN true" 0 3084
run_case "already merged is not evicted" 0 "evicted=false" "$queue_ref" "MERGED false" 0 3084
run_case "open but out of the queue is evicted" 0 "evicted=true" "$queue_ref" "OPEN false" 0 3084
run_case "closed without merging is evicted" 0 "evicted=true" "$queue_ref" "CLOSED false" 0 3084
run_case "failed read writes nothing" 1 "" "$queue_ref" "OPEN false" 1
run_case "missing PR writes nothing" 1 "" "$queue_ref" "null null" 0
run_case "unknown state writes nothing" 1 "" "$queue_ref" "OPEN maybe" 0
run_case "non-queue ref writes nothing" 1 "" "refs/heads/main" "OPEN false" 0
run_case "short head sha is refused" 1 "" "refs/heads/gh-readonly-queue/main/pr-3084-abc123" "OPEN false" 0
run_case "PR number zero is refused" 1 "" "refs/heads/gh-readonly-queue/main/pr-0-$sha" "OPEN false" 0
run_case "base branch with a slash" 0 "evicted=true" "refs/heads/gh-readonly-queue/release/v1/pr-42-$sha" "OPEN false" 0 42

echo "merge-group-evicted: $assertions assertions, $failures failed"
[ "$failures" = 0 ]
