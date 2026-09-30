#!/usr/bin/env bash
#
# Pins scripts/prod-stranded-gate.sh against a stub `gh` command.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/prod-stranded-gate.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

[ -x "$script" ] || { printf 'FAIL: %s is not executable\n' "$script"; exit 1; }

mkdir -p "$scratch/bin"
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
case "$1 $2" in
  "api graphql")
    [ "${STUB_QUEUE_RC:-0}" = 0 ] || exit "$STUB_QUEUE_RC"
    printf '%s\n' "$STUB_QUEUE"
    ;;
  "run list")
    [ "${STUB_RUNS_RC:-0}" = 0 ] || exit "$STUB_RUNS_RC"
    printf '%s\n' "$STUB_RUNS"
    ;;
  *) exit 99 ;;
esac
EOF
chmod +x "$scratch/bin/gh"

# <label> <expected-rc> <expected-output: true|false|none> <queue> <queue-rc> <runs> <runs-rc>
run_case() {
  local label="$1" want_rc="$2" want_answer="$3" out rc answer
  : >"$scratch/log"
  : >"$scratch/output"
  if out="$(PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" \
    STUB_QUEUE="$4" STUB_QUEUE_RC="$5" STUB_RUNS="$6" STUB_RUNS_RC="$7" \
    STRANDED_REPOSITORY=devantler-tech/platform GITHUB_OUTPUT="$scratch/output" \
    "$script" 2>&1)"; then rc=0; else rc=$?; fi
  answer="$(sed -n 's/^stranded=//p' "$scratch/output")"
  [ -n "$answer" ] || answer=none

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [ "$answer" = "$want_answer" ]; then
    printf '  ok   %s (exit %s, stranded=%s)\n' "$label" "$rc" "$answer"
  else
    printf '  FAIL %s: expected exit %s, stranded=%s; got exit %s, stranded=%s:\n%s\n' \
      "$label" "$want_rc" "$want_answer" "$rc" "$answer" "$out"
    failures=$((failures + 1))
  fi
}

run_case "an idle queue with no unfinished run is stranded" 0 true 0 0 0 0
run_case "a queued group is not stranded" 0 false 1 0 0 0
run_case "an unfinished merge-group run is not stranded" 0 false 0 0 1 0
run_case "a busy queue and a run are not stranded" 0 false 3 0 2 0
run_case "an unreadable queue decides nothing" 1 none "" 1 0 0
run_case "a missing queue (null) decides nothing" 1 none none 0 0 0
run_case "an unreadable run list decides nothing" 1 none 0 0 "" 1
run_case "a malformed run count decides nothing" 1 none 0 0 "null" 0

# The run list must be scoped to merge-group runs of ci.yaml: an unrelated unfinished run
# (a pull_request CI, a CD) is not a group that could still merge.
assertions=$((assertions + 1))
: >"$scratch/log"
PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_QUEUE=0 STUB_RUNS=0 \
  STRANDED_REPOSITORY=devantler-tech/platform GITHUB_OUTPUT=/dev/null "$script" >/dev/null 2>&1
# shellcheck disable=SC2016  # the literal GraphQL variable is what the query carries.
if grep -Fq 'gh run list --repo devantler-tech/platform --workflow ci.yaml --event merge_group' "$scratch/log" &&
  grep -Fq 'mergeQueue(branch:$base)' "$scratch/log" && grep -Fq 'base=main' "$scratch/log"; then
  printf '  ok   reads the main queue and only merge-group runs of ci.yaml\n'
else
  printf '  FAIL the queries are not scoped as expected:\n%s\n' "$(cat "$scratch/log")"
  failures=$((failures + 1))
fi

assertions=$((assertions + 1))
if out="$(PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_QUEUE=0 STUB_RUNS=0 \
  STRANDED_REPOSITORY='not-a-repo' GITHUB_OUTPUT=/dev/null "$script" 2>&1)"; then
  printf '  FAIL a malformed repository was accepted:\n%s\n' "$out"
  failures=$((failures + 1))
else
  printf '  ok   a malformed repository fails before any read\n'
fi

printf '%s assertions, %s failures\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
