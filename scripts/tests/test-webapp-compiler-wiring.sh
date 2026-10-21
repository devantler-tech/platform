#!/usr/bin/env bash
# Execute the real required-result boundary; a skipped selected compiler is not success.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ci="$(yq -o=json '.' "${root}/.github/workflows/ci.yaml")"
main="$(yq -o=json '.' "${root}/.github/workflows/validate-main.yaml")"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

check_job() {
  local input="$1" selection="$2"
  jq -e --arg selection "${selection}" '
    .jobs["validate-webapp-compiler"] as $job
    | $job != null and $job.permissions == {"contents":"read"}
      and ($job["continue-on-error"] // false) == false
      and ($job["timeout-minutes"] > 0 and $job["timeout-minutes"] <= 15)
      and (if $selection == "" then ($job | has("if") | not)
           else $job.if == $selection and $job.needs == ["changes"] end)
      and ([ $job.steps[] | select((.run // "") | contains("bash scripts/tests/test-webapp-compiler.sh")) ]
           | length == 1 and (.[0] | has("if") | not)
             and (.[0]["continue-on-error"] // false) == false
             and .[0].run == "bash scripts/tests/test-webapp-compiler.sh")
  ' <<<"${input}" >/dev/null || fail 'compiler job is absent, bypassed or unbounded'
}
check_job "${ci}" "needs.changes.outputs.k8s == 'true'"
check_job "${main}" ""
jq -e '
  (.jobs["deploy-prod"].needs | index("validate-webapp-compiler") != null)
  and (.jobs["ci-required-checks"].needs | index("validate-webapp-compiler") != null)
  and any(.jobs["ci-required-checks"].steps[];
    (.with["job-results"] // "") | contains("${{ needs.validate-webapp-compiler.result }}"))
  and (.jobs["validate-repository-guards"] | has("if") | not)
  and (.jobs["validate-repository-guards"]["continue-on-error"] // false) == false
  and ([.jobs["validate-repository-guards"].steps[] |
    select((.run // "") | contains("bash scripts/tests/test-webapp-compiler-wiring.sh"))] |
    length == 1 and (.[0] | has("if") | not)
      and (.[0]["continue-on-error"] // false) == false
      and .[0].run == "shellcheck scripts/tests/test-webapp-compiler.sh scripts/tests/test-webapp-compiler-wiring.sh\nbash scripts/tests/test-webapp-compiler-wiring.sh\n")
' <<<"${ci}" >/dev/null || fail 'compiler result must gate deployment and required CI independently'
filter="$(yq -o=json '.jobs.changes.steps[] | select(.id == "filter") | .with.filters | from_yaml | .k8s' "${root}/.github/workflows/ci.yaml")"
jq -e '
  index("scripts/tests/test-webapp-compiler.sh") != null
  and index("scripts/tests/test-webapp-compiler-wiring.sh") != null
  and index("scripts/tests/webapp-compiler/**") != null
' <<<"${filter}" >/dev/null || fail 'compiler inputs must select validation'
step="$(jq -c '[.jobs["ci-required-checks"].steps[] |
  select(.env.COMPILER_RESULT == "${{ needs.validate-webapp-compiler.result }}")]' <<<"${ci}")"
jq -e 'length == 1 and (.[0] | has("if") | not)
  and (.[0]["continue-on-error"] // false) == false
  and .[0].env.EVENT_NAME == "${{ github.event_name }}"
  and .[0].env.SELECTED == "${{ needs.changes.outputs.k8s }}"
  and (.[0].run | type == "string" and length > 0)' <<<"${step}" >/dev/null ||
  fail 'required compiler result boundary is missing or bypassed'
jq -r '.[0].run' <<<"${step}" >"${work}/gate.sh"
check() {
  local event="$1" selected="$2" result="$3" expected="$4" actual=0
  EVENT_NAME="${event}" SELECTED="${selected}" COMPILER_RESULT="${result}" \
    bash -e "${work}/gate.sh" >"${work}/result.log" 2>&1 || actual=$?
  [[ "${actual}" == "${expected}" ]] || fail "unexpected result for ${event}/${selected}/${result}"
}
for event in pull_request merge_group; do
  check "${event}" true success 0
  for result in failure cancelled skipped pending unknown ''; do
    check "${event}" true "${result}" 1
  done
  check "${event}" false skipped 0
  check "${event}" false failure 1
  check "${event}" '' skipped 1
  check "${event}" unexpected skipped 1
done
check push true success 1
nonexecuting="$(jq '(.jobs["validate-webapp-compiler"].steps[] |
  select((.run // "") | contains("test-webapp-compiler.sh"))).run =
  "echo bash scripts/tests/test-webapp-compiler.sh"' <<<"${ci}")"
if (check_job "${nonexecuting}" "needs.changes.outputs.k8s == 'true'") >"${work}/nonexecuting.log" 2>&1; then
  fail 'printing the compiler command must not count as executing it'
fi
echo 'PASS: selected KRO compiler proof is required before merge and deploy'
