#!/usr/bin/env bash
#
# Pins scripts/redeploy-main-when-behind.sh against stub `gh` and `git` commands.
#
# The stub run list answers from a sequence of snapshots: the first call is the list
# before any dispatch, later calls are the lists seen while waiting for the new run.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/redeploy-main-when-behind.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

[ -x "$script" ] || { printf 'FAIL: %s is not executable\n' "$script"; exit 1; }

tip="$(printf 'a%.0s' {1..40})"
older="$(printf 'b%.0s' {1..40})"

mkdir -p "$scratch/bin"
cat >"$scratch/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >>"$STUB_LOG"
case "$1 $2" in
  "run list")
    [ "${STUB_LIST_RC:-0}" = 0 ] || exit "$STUB_LIST_RC"
    calls=$(( $(cat "$STUB_CALLS") + 1 )); printf '%s' "$calls" >"$STUB_CALLS"
    if [ "$calls" = 1 ]; then printf '%b' "$STUB_BEFORE"; else printf '%b' "$STUB_AFTER"; fi
    ;;
  "workflow run") exit "${STUB_DISPATCH_RC:-0}" ;;
  *) exit 99 ;;
esac
EOF
cat >"$scratch/bin/git" <<'EOF'
#!/usr/bin/env bash
printf 'git %s\n' "$*" >>"$STUB_LOG"
[ -n "${STUB_TIP:-}" ] || exit 1
printf '%s\n' "$STUB_TIP"
EOF
chmod +x "$scratch/bin/gh" "$scratch/bin/git"

# <label> <expected-rc> <expected-annotation> <tip> <before> <after> <list-rc> <dispatch-rc> <dispatch-expected: yes|no>
run_case() {
  local label="$1" want_rc="$2" want_annotation="$3" want_dispatch="$9" out rc dispatched=no
  : >"$scratch/log"
  printf '0' >"$scratch/calls"
  if out="$(PATH="$scratch/bin:$PATH" STUB_LOG="$scratch/log" STUB_CALLS="$scratch/calls" \
    STUB_TIP="$4" STUB_BEFORE="$5" STUB_AFTER="$6" STUB_LIST_RC="$7" STUB_DISPATCH_RC="$8" \
    REDEPLOY_REPOSITORY=devantler-tech/platform REDEPLOY_POLL_ATTEMPTS=3 REDEPLOY_POLL_INTERVAL=0 \
    "$script" 2>&1)"; then rc=0; else rc=$?; fi
  grep -Fq 'gh workflow run cd.yaml --repo devantler-tech/platform --ref main' "$scratch/log" && dispatched=yes

  assertions=$((assertions + 1))
  if [ "$rc" = "$want_rc" ] && [[ "$out" == *"$want_annotation"* ]] && [ "$dispatched" = "$want_dispatch" ]; then
    printf '  ok   %s (exit %s, dispatched=%s)\n' "$label" "$rc" "$dispatched"
  else
    printf '  FAIL %s: expected exit %s, %s, dispatched=%s; got exit %s, dispatched=%s:\n%s\n' \
      "$label" "$want_rc" "$want_annotation" "$want_dispatch" "$rc" "$dispatched" "$out"
    failures=$((failures + 1))
  fi
}

run_case "no pending run dispatches and waits for the new run" 0 "::notice title=Redeploying main::" \
  "$tip" "" "101 $tip\n" 0 0 yes
run_case "a pending run for the tip is not duplicated" 0 "::notice title=Redeploy already pending::" \
  "$tip" "100 $tip\n" "100 $tip\n" 0 0 no
run_case "a pending run for an older commit does not count" 0 "::notice title=Redeploying main::" \
  "$tip" "100 $older\n" "100 $older\n101 $tip\n" 0 0 yes
run_case "the old run alone never confirms the dispatch" 1 "no new CD run appeared" \
  "$tip" "100 $older\n" "100 $older\n" 0 0 yes
run_case "an unresolvable tip fails before listing or dispatching" 1 "could not resolve" \
  "" "" "" 0 0 no
run_case "an unreadable run list fails closed without dispatching" 1 "could not list CD runs" \
  "$tip" "" "" 1 0 no
run_case "a refused dispatch fails" 1 "could not dispatch" \
  "$tip" "" "" 0 1 yes

printf '%s assertions, %s failures\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
