#!/usr/bin/env bash
# Exercise the proof's report reader with complete, stale and ambiguous evidence.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
reader="$root/tests/kyverno-stale-report-prune/assert-probe-result.sh"
fixture='{"metadata":{"uid":"report-1"},"scope":{"uid":"resource-1"},"results":[{"policy":"require-owner-label-unevaluated","rule":"owner-label","result":"skip","timestamp":{"seconds":200}}]}'
passed=0
failed=0
check() {
  local name="$1" want="$2" input="$3" since="${4:-200}" verdict="${5:-skip}" actual=0
  bash "$reader" resource-1 report-1 "$since" "$verdict" <<<"$input" >/dev/null 2>&1 || actual=$?
  if [[ "$actual" == "$want" ]]; then
    printf 'PASS %s\n' "$name"
    passed=$((passed + 1))
  else
    printf 'FAIL %s: wanted exit %s, got %s\n' "$name" "$want" "$actual"
    failed=$((failed + 1))
  fi
}
check 'fresh skip joined to the same report and resource' 0 "$fixture"
check 'fresh failure remains a valid unexempted control' 0 "$(jq '.results[0].result="fail"' <<<"$fixture")" 200 fail
check 'other policy results do not contaminate the target rule' 0 "$(jq '.results += [{policy:"other-policy",rule:"other-rule",result:"fail",timestamp:{seconds:200}}]' <<<"$fixture")"
check 'a stale skip cannot prove an updated exemption' 1 "$fixture" 201
check 'an old failure cannot prove a skip' 1 "$(jq '.results[0].result="fail"' <<<"$fixture")"
check 'a pass cannot prove that the exemption ran' 1 "$(jq '.results[0].result="pass"' <<<"$fixture")"
check 'a replacement report cannot prove in-place refresh' 1 "$(jq '.metadata.uid="report-2"' <<<"$fixture")"
check 'a report for another resource cannot prove coverage' 1 "$(jq '.scope.uid="resource-2"' <<<"$fixture")"
check 'a simultaneous old failure and fresh skip is ambiguous' 1 "$(jq '.results += [.results[0] | .result="fail" | .timestamp.seconds=100]' <<<"$fixture")"
check 'a duplicate skip is ambiguous' 1 "$(jq '.results += [.results[0]]' <<<"$fixture")"
check 'a missing target result is incomplete' 1 "$(jq '.results=[]' <<<"$fixture")"
check 'a missing timestamp is incomplete' 1 "$(jq 'del(.results[0].timestamp)' <<<"$fixture")"
check 'a string timestamp is malformed' 1 "$(jq '.results[0].timestamp.seconds="200"' <<<"$fixture")"
check 'a zero timestamp is not a completed scan' 1 "$(jq '.results[0].timestamp.seconds=0' <<<"$fixture")" 1
check 'a fractional timestamp is malformed' 1 "$(jq '.results[0].timestamp.seconds=200.5' <<<"$fixture")"
check 'an empty read is incomplete' 1 ''
check 'partial JSON is incomplete' 1 '{"metadata":'
check 'two complete documents are not one observation' 1 "$fixture
$fixture"
check 'an array is not the expected report object' 1 "[$fixture]"
check 'a missing resource identity is incomplete' 1 "$(jq 'del(.scope)' <<<"$fixture")"
check 'a result object is not the required array' 1 "$(jq '.results={first:.results[0]}' <<<"$fixture")"
check 'a future timestamp cannot prove a completed scan' 1 "$(jq '.results[0].timestamp.seconds=999999999999' <<<"$fixture")"
printf '%s passed, %s failed\n' "$passed" "$failed"
[[ "$failed" == 0 ]]
