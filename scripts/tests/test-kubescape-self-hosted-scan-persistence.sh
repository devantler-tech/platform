#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly helm_release="${root_dir}/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the Kubescape HelmRelease'

scanner_tag="$(yq -er '.spec.values.kubescape.image.tag | select(tag == "!!str")' "${helm_release}")" ||
  fail 'the Kubescape scanner image tag is missing or is not a string'
readonly scanner_tag

[[ "${scanner_tag}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
  fail "Kubescape scanner tag ${scanner_tag} is not an exact vMAJOR.MINOR.PATCH release"
readonly major="${BASH_REMATCH[1]}"
readonly minor="${BASH_REMATCH[2]}"
readonly patch="${BASH_REMATCH[3]}"

# Kubescape <=4.0.11 can override an explicit local-only request, enter SaaS
# submission with no backend configured, and abort on account-ID chmod before
# its in-cluster report receiver persists WorkloadConfigurationScan objects.
# Upstream kubescape#2556 fixes all three links and first ships in v4.0.12.
if ((major < 4 || (major == 4 && minor == 0 && patch < 12))); then
  fail "Kubescape scanner ${scanner_tag} predates v4.0.12 and can silently drop self-hosted posture results"
fi

keep_local="$(yq -er '
  .spec.values.kubescapeScheduler.requestBody.commands[] |
  select(.CommandName == "kubescapeScan") |
  .args.scanV1.keepLocal |
  select(tag == "!!bool")
' "${helm_release}")" || fail 'the scheduled Kubescape scan has no explicit keepLocal setting'
readonly keep_local
[[ "${keep_local}" == "true" ]] ||
  fail 'the scheduled Kubescape scan must remain local-only in this self-hosted deployment'

offline="$(yq -er '.spec.values.capabilities.kubescapeOffline' "${helm_release}")" ||
  fail 'capabilities.kubescapeOffline is missing'
readonly offline
[[ "${offline}" == "disable" ]] ||
  fail 'Kubescape offline mode must stay disabled so the scanner can fetch policy artifacts'

# The scanner's API-server persistence handler stores detailed
# WorkloadConfigurationScan objects only when clusterData.continuousPostureScan
# is true. Chart 1.40.3 derives that flag from this capability.
continuous_scan="$(yq -er '.spec.values.capabilities.continuousScan' "${helm_release}")" ||
  fail 'capabilities.continuousScan is missing'
readonly continuous_scan
[[ "${continuous_scan}" == "enable" ]] ||
  fail 'continuousScan must be enabled so scheduled scans persist detailed posture results'

# Enabling the capability also starts the operator's event-driven scanner. Keep
# its resource set empty: the pinned operator turns an empty match list into an
# empty watch pool, preserving scheduled persistence without scan-on-change
# traffic that omits keepLocal.
continuous_match_count="$(yq -er '
  .spec.values.continuousScanning.matchingRules.match |
  select(tag == "!!seq") |
  length
' "${helm_release}")" || fail 'continuous-scanning matchingRules.match must be an explicit list'
readonly continuous_match_count
[[ "${continuous_match_count}" == "0" ]] ||
  fail 'continuous-scanning matchingRules.match must stay empty in this self-hosted deployment'

continuous_namespace_count="$(yq -er '
  .spec.values.continuousScanning.matchingRules.namespaces |
  select(tag == "!!seq") |
  length
' "${helm_release}")" || fail 'continuous-scanning matchingRules.namespaces must be an explicit list'
readonly continuous_namespace_count
[[ "${continuous_namespace_count}" == "0" ]] ||
  fail 'continuous-scanning matchingRules.namespaces must stay empty in this self-hosted deployment'

printf 'Kubescape self-hosted scan persistence contract is valid (%s).\n' "${scanner_tag}"
