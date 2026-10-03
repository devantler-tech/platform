#!/usr/bin/env bash
# Exercise the live-rules consumer: moved workflow paths must change the result,
# and incomplete or malformed reads must never become a missing-workflow verdict.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "${FAIL_READ:-0}" == 0 ]] || exit 47
case "$*" in
  'api repos/devantler-tech/platform/rules/branches/main --paginate --slurp') cat "$RULES" ;;
  "api --paginate --slurp repos/devantler-tech/platform/actions/runs?head_sha=${TEST_HEAD}&per_page=100")
    cat "$RUNS"
    [[ "${PARTIAL_READ:-0}" == 0 ]] || exit 47
    ;;
  *) exit 99 ;;
esac
EOF
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH" RULES="$work/rules.json" RUNS="$work/runs.json"
export TEST_HEAD=0123456789012345678901234567890123456789
fail() { echo "$*" >&2; exit 1; }
fixture() {
  jq -n --arg path "$1" '[[{type:"required_status_checks",parameters:{required_status_checks:[{context:"CI - Required Checks"}]} }],[{type:"workflows",ruleset_id:12,ruleset_source_type:"Organization",ruleset_source:"devantler-tech",parameters:{workflows:[{path:$path,repository_id:948529001,ref:"refs/heads/main"}]}}]]' > "$RULES"
}
fixture .github/workflows/dependency-review.yaml
output="$(bash "$root/scripts/required-merge-workflows.sh")"
jq -e 'length==1 and .[0].path==".github/workflows/dependency-review.yaml" and .[0].repository_id==948529001 and .[0].ref=="refs/heads/main" and .[0].ruleset_id==12' <<< "$output" >/dev/null || fail 'later-page required workflow or source identity was lost'
fixture .github/workflows/replacement.yaml
output="$(bash "$root/scripts/required-merge-workflows.sh")"
jq -e 'length==1 and .[0].path==".github/workflows/replacement.yaml"' <<< "$output" >/dev/null || fail 'source-model drift left a stale workflow path'
fixture .github/workflows/dependency-review.yaml
if FAIL_READ=1 bash "$root/scripts/required-merge-workflows.sh" > "$work/output" 2> "$work/error"; then fail 'failed API read passed'; fi
[[ ! -s "$work/output" ]] || fail 'failed read emitted a usable result'
for mutation in \
  '.[1][0].parameters.workflows[0].repository_id=null' \
  '.[1][0].parameters.workflows[0].path=".github/workflows/../other.yaml"' \
  '.[1][0].parameters.workflows[0].ref=null' \
  '.[1][0].parameters.workflows=[]' \
  '.[1][0].ruleset_id=null' \
  '.[1][0].parameters=null'; do
  fixture .github/workflows/dependency-review.yaml
  jq "$mutation" "$RULES" > "$work/mutated.json"
  mv "$work/mutated.json" "$RULES"
  if bash "$root/scripts/required-merge-workflows.sh" > "$work/output" 2> "$work/error"; then fail "malformed rule passed: $mutation"; fi
  [[ ! -s "$work/output" ]] || fail 'malformed rule emitted a usable result'
done
printf '[]\n' > "$RULES"
if bash "$root/scripts/required-merge-workflows.sh" > "$work/output" 2> "$work/error"; then fail 'no page at all passed'; fi
printf '[[{"type":"merge_queue","parameters":{}}]]\n' > "$RULES"
output="$(bash "$root/scripts/required-merge-workflows.sh")"
[[ "$output" == '[]' ]] || fail 'a complete source without workflow rules invented a requirement'

# Consume the actual runbook query blocks against a changed live source model.
# Reintroducing a stale literal in either query or the poll must fail this test.
runbook="${RUNBOOK_PATH:-$root/docs/operations/merge-queue-blocked.md}"
extract_block() {
  local heading="$1" destination="$2"
  awk -v heading="$heading" '
    index($0, heading)==1 {wanted=1}
    wanted && /^```(sh|bash)$/ {inside=1; next}
    inside && /^```$/ {exit}
    inside {print}
  ' "$runbook" | sed "s/<head>/$TEST_HEAD/g" > "$destination"
  [[ -s "$destination" ]] || fail "runbook query missing: $heading"
}
extract_block '**Did it fire at all?**' "$work/count.sh"
extract_block '**Is the requirement satisfied?**' "$work/success.sh"
extract_block '**Run this one with Bash specifically**' "$work/poll.sh"
fixture .github/workflows/replacement.yaml
export managed
managed="$(bash "$root/scripts/required-merge-workflows.sh" | jq -er '.[0].path')"
export new_head="$TEST_HEAD"
jq -n --arg path "$managed" '[{workflow_runs:[{path:".github/workflows/unrelated.yaml",status:"completed",conclusion:"success"},{path:$path,status:"completed",conclusion:"failure"}]},{workflow_runs:[{path:$path,status:"completed",conclusion:"success"}]}]' > "$RUNS"
[[ "$(bash "$work/count.sh")" == 2 ]] || fail 'actual runbook count used a stale path or lost a page'
[[ "$(bash "$work/success.sh")" == 1 ]] || fail 'actual runbook success query used a stale path or accepted a failed run'
for block in count success; do
  if PARTIAL_READ=1 bash "$work/$block.sh" > "$work/output" 2> "$work/error"; then fail "actual runbook $block query accepted a partial API read"; fi
  [[ ! -s "$work/output" ]] || fail "actual runbook $block query emitted a usable partial result"
done
cat > "$work/bin/seq" <<'EOF'
#!/usr/bin/env bash
[[ "$*" == '1 30' ]] || exit 99
printf '1\n'
EOF
cat > "$work/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$work/bin/seq" "$work/bin/sleep"
# shellcheck disable=SC2016 # These variables belong to the runbook subprocess.
printf '\nprintf "%%s|%%s\\n" "$managed_run" "$managed_conclusion"\n' >> "$work/poll.sh"
[[ "$(bash "$work/poll.sh")" == '1|success' ]] || fail 'actual runbook poll used a stale path or stopped before a verdict'
[[ "$(PARTIAL_READ=1 bash "$work/poll.sh")" == '|' ]] || fail 'actual runbook poll accepted a partial API read'
jq -n --arg path "$managed" '[{workflow_runs:[{path:$path,status:"in_progress",conclusion:null}]}]' > "$RUNS"
[[ "$(bash "$work/success.sh")" == 0 ]] || fail 'actual runbook strict query accepted a running run'
[[ "$(bash "$work/poll.sh")" == '|' ]] || fail 'actual runbook poll called a running run failed or satisfied'
jq -n --arg path "$managed" '[{workflow_runs:[{path:$path,status:"completed",conclusion:"failure"}]}]' > "$RUNS"
[[ "$(bash "$work/poll.sh")" == '1|failed' ]] || fail 'actual runbook poll hid a genuinely failed run'
printf '[{"workflow_runs":[]}]\n' > "$RUNS"
[[ "$(bash "$work/poll.sh")" == '|' ]] || fail 'actual runbook poll inferred failure or satisfaction from absence'
if FAIL_READ=1 bash "$work/count.sh" >/dev/null 2>&1; then fail 'actual runbook query accepted a failed API read'; fi
printf 'Required merge-workflow source consumer passed.\n'
