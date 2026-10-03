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
[[ "$*" == 'api repos/devantler-tech/platform/rules/branches/main --paginate --slurp' ]] || exit 99
[[ "${FAIL_READ:-0}" == 0 ]] || exit 47
cat "$RULES"
EOF
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH" RULES="$work/rules.json"
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
printf 'Required merge-workflow source consumer passed.\n'
