#!/usr/bin/env bash
# Exercise native admission and the real scanner; no cluster or credentials.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$repo_root"
policy=k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-openbao-listener.yaml
listener=k8s/providers/hetzner/infrastructure/controllers/openbao/transport/config-map-listener.yaml
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
fail() { printf 'ARC public listener: FAIL %s\n' "$1" >&2; exit 1; }
command -v ksail >/dev/null 2>&1 || fail missing-native-scanner
yq -o=json '.' "$listener" >"$scratch/public.json"
kyverno apply "$policy" --resource "$scratch/public.json" --remove-color >"$scratch/admission" 2>&1 || fail public-settings
grep -Fq 'pass: 1, fail: 0, warn: 0, error: 0, skip: 0' "$scratch/admission" || fail unevaluated-positive

for change in \
  '.data["listener.hcl"] += "synthetic PRIVATE KEY content\n"' \
  '.data["listener.hcl"] |= sub("/openbao/arc-tls/tls.key"; "synthetic PRIVATE KEY")' \
  '.data.password = "synthetic"' \
  '.binaryData = {credential:"c3ludGhldGlj"}' \
  '.metadata.namespace = "elsewhere"' \
  'del(.data)' \
  '.data["listener.hcl"] += "\n"' \
  '.data["listener.hcl"] |= sub("tls12"; "tls10")'; do
  jq "$change" "$scratch/public.json" >"$scratch/denied.json"
  if kyverno apply "$policy" --resource "$scratch/denied.json" --remove-color >"$scratch/admission" 2>&1; then
    fail admitted-negative
  fi
  grep -Fq 'pass: 0, fail: 1, warn: 0, error: 0, skip: 0' "$scratch/admission" || fail unevaluated-negative
done

go run ./scripts/generate-kubescape-exceptions -o "$scratch/exceptions.json" >"$scratch/generator" 2>&1
scan() {
  ksail workload scan --no-render --framework nsa,mitre --format json --output "$scratch/$1.json" \
    "${@:2}" >"$scratch/scanner" 2>&1 || fail scanner-execution
}
scan baseline "$listener"
jq -e '.summaryDetails.controls[] | select(.controlID == "C-0012") |
  .ResourceCounters.failedResources == 1 and .subStatusCounters.ignoredResources == 0' \
  "$scratch/baseline.json" >/dev/null || fail missing-false-positive
scan justified --exceptions "$scratch/exceptions.json" "$listener"
jq -e '.summaryDetails.controls[] | select(.controlID == "C-0012") |
  .ResourceCounters.failedResources == 0 and .subStatusCounters.ignoredResources == 1' \
  "$scratch/justified.json" >/dev/null || fail unapplied-disposition
scan mirror k8s/bases/infrastructure/controllers/kubescape/config-map-headlamp-exceptions.yaml
jq -e '.summaryDetails.controls[] | select(.controlID == "C-0012") |
  .ResourceCounters.passedResources == 1 and .ResourceCounters.failedResources == 0 and .subStatusCounters.ignoredResources == 0' \
  "$scratch/mirror.json" >/dev/null || fail new-mirror-false-positive
jq '.metadata.name = "synthetic-credential-control" | .data = {password:"synthetic PRIVATE KEY"}' \
  "$scratch/public.json" >"$scratch/credential-input.json"
scan credential --exceptions "$scratch/exceptions.json" "$scratch/credential-input.json"
jq -e '.summaryDetails.controls[] | select(.controlID == "C-0012") |
  .ResourceCounters.failedResources == 1 and .subStatusCounters.ignoredResources == 0' \
  "$scratch/credential.json" >/dev/null || fail hidden-genuine-credential
printf 'ARC public listener: native admission 1 pass/8 denies; scanner false positive, exact disposition, unexcepted mirror and credential control verified.\n'
