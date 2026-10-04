#!/usr/bin/env bash
# Exercise the actual recovery-runbook query without a cluster.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
runbook="${1:-${root_dir}/docs/dr/runbook.md}"
filter="$(awk '
  /^# 2\. Replica scheduling/ { in_check=1 }
  in_check && /^  jq -r / { in_filter=1; sub(/^  jq -r \047/, ""); found++ }
  in_filter {
    if (sub(/\047$/, "")) { print; in_filter=0; in_check=0 } else print
  }
  END { if (found != 1 || in_filter) exit 1 }
' "${runbook}")"
[[ -n "${filter}" ]] || { echo 'Missing runbook disk query' >&2; exit 1; }

check() {
  local label="$1" input="$2" expected="$3" actual
  actual="$(jq -r "${filter}" <<<"${input}")"
  if [[ "${actual}" != "${expected}" ]]; then
    printf 'FAIL: %s\nExpected:\n%s\nActual:\n%s\n' "${label}" "${expected}" "${actual}" >&2
    exit 1
  fi
  printf 'PASS: %s\n' "${label}"
}

check 'a constrained second disk is visible' \
  '{"items":[{"metadata":{"name":"node-a"},"status":{"diskStatus":{"first":{"storageAvailable":80,"storageMaximum":100,"conditions":[{"type":"Schedulable","status":"True"}]},"second":{"storageAvailable":24,"storageMaximum":100,"conditions":[{"type":"Schedulable","status":"False"}]}}}}]}' \
  $'node-a disk=first avail=80% Schedulable=True\nnode-a disk=second avail=24% Schedulable=False'
check 'single-disk availability and scheduling are preserved' \
  '{"items":[{"metadata":{"name":"node-b"},"status":{"diskStatus":{"data":{"storageAvailable":37,"storageMaximum":100,"conditions":[{"type":"Schedulable","status":"True"}]}}}}]}' \
  'node-b disk=data avail=37% Schedulable=True'
check 'zero capacity is unknown, never healthy or division by zero' \
  '{"items":[{"metadata":{"name":"node-c"},"status":{"diskStatus":{"data":{"storageAvailable":0,"storageMaximum":0}}}}]}' \
  'node-c disk=data avail=UNKNOWN Schedulable=UNKNOWN'
check 'missing available capacity is unknown' \
  '{"items":[{"metadata":{"name":"node-d"},"status":{"diskStatus":{"data":{"storageMaximum":100}}}}]}' \
  'node-d disk=data avail=UNKNOWN Schedulable=UNKNOWN'
check 'a node with no disk status is visible as unknown' \
  '{"items":[{"metadata":{"name":"node-e"},"status":{}}]}' \
  'node-e disk=UNKNOWN avail=UNKNOWN Schedulable=UNKNOWN'
