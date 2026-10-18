#!/usr/bin/env bash
# Exercise the required aggregate's real policy-result boundary. Removing its
# prerequisite or accepting a selected non-success must fail this regression.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="${root}/.github/workflows/ci.yaml"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

gate="$(yq -o=json '.jobs.ci-required-checks' "${workflow}")"
if ! jq -e '.needs | index("validate-cluster-policies") != null' <<<"${gate}" >/dev/null; then
  echo 'FAIL: required aggregate must wait for the independent policy job' >&2
  exit 1
fi
step="$(jq -c '[.steps[] | select(.name == "⚖️ Require selected policy validation")]' <<<"${gate}")"
jq -e 'length == 1 and (.[0].run | type == "string" and length > 0)' <<<"${step}" >/dev/null
jq -r '.[0].run' <<<"${step}" >"${work}/gate.sh"
# Check the authored boundary mappings, not only an isolated copy of its shell.
jq -e '.[0].env == {
  EVENT_NAME: "${{ github.event_name }}",
  SELECTED: "${{ needs.changes.outputs.k8s }}",
  POLICY_RESULT: "${{ needs.validate-cluster-policies.result }}"
}' <<<"${step}" >/dev/null

check() {
  local event="$1" selected="$2" result="$3" expected="$4" actual=0
  EVENT_NAME="${event}" SELECTED="${selected}" POLICY_RESULT="${result}" \
    bash -e "${work}/gate.sh" >"${work}/output" 2>&1 || actual=$?
  if [ "${actual}" -ne "${expected}" ]; then
    printf 'FAIL: event=%s selected=%s result=%s expected=%s actual=%s\n' \
      "${event}" "${selected}" "${result}" "${expected}" "${actual}" >&2
    cat "${work}/output" >&2
    exit 1
  fi
}

check pull_request true success 0
for result in failure cancelled skipped pending unknown ''; do
  check pull_request true "${result}" 1
done
check pull_request false skipped 0
check pull_request '' skipped 1
check pull_request unexpected skipped 1
check merge_group true skipped 0
check merge_group false skipped 0

echo 'PASS: selected policy validation must succeed; intentional non-selection is accepted'
