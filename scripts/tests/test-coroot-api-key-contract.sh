#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production overlays'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to convert the rendered resources'
command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect the rendered resources'

# Render an overlay into one JSON array so every assertion is a single jq
# program over the whole document set.
# shellcheck disable=SC2016 # $item is a yq variable, not a shell expansion
render_json() {
  kubectl kustomize "$1" |
    yq ea -o=json -I=0 '. as $item ireduce ([]; . + [$item])' -
}

infrastructure="$(render_json "${root_dir}/k8s/providers/hetzner/infrastructure")" ||
  fail 'the production infrastructure overlay must render successfully'
controllers="$(render_json "${root_dir}/k8s/providers/hetzner/infrastructure/controllers")" ||
  fail 'the production infrastructure-controllers overlay must render successfully'

assert() {
  local document="$1" program="$2" message="$3"
  printf '%s\n' "${document}" | jq -e "${program}" >/dev/null || fail "${message}"
}

coroot='map(select(.kind == "Coroot" and .metadata.namespace == "observability" and .metadata.name == "coroot"))'

# One key for everything that talks to Coroot. The operator generates the
# project key Secret; the bundled agents and the audit-log forwarder must both
# read that same Secret, or the agents send telemetry with an empty API_KEY.
assert "${infrastructure}" "${coroot} | length == 1" \
  'the production overlay must render exactly one observability/coroot Coroot resource'

assert "${infrastructure}" "${coroot} | .[0].spec.apiKeySecret == {\"name\": \"coroot-api-key\", \"key\": \"key\"}" \
  'the Coroot resource must wire the bundled agents to coroot-api-key/key via spec.apiKeySecret'

assert "${infrastructure}" \
  "${coroot} | [.[0].spec.projects[]?.apiKeys[]?.keySecret | select(. == {\"name\": \"coroot-api-key\", \"key\": \"key\"})] | length == 1" \
  'exactly one Coroot project API key must be generated into coroot-api-key/key'

assert "${infrastructure}" \
  '[.[] | select(.kind == "HelmRelease") | .spec.values.extraEnvs[]? | select(.name == "COROOT_API_KEY") | .valueFrom.secretKeyRef] == [{"name": "coroot-api-key", "key": "key"}]' \
  'the audit-log forwarder must read COROOT_API_KEY from coroot-api-key/key'

# Component images stay pinned. The operator lists the registry only when an
# image is unset, so pins remain the primary path and GHCR egress the fallback.
# The node-agent may use either Coroot's upstream image or the platform's
# signed compatibility build; both repositories are explicit allow-list entries.
assert "${infrastructure}" \
  "${coroot} | .[0].spec |
    (.communityEdition.image.name | test(\"^ghcr[.]io/coroot/coroot:[0-9]\")) and
    (.nodeAgent.image.name | test(\"^ghcr[.]io/(coroot/coroot-node-agent:[0-9]|devantler-tech/platform-coroot-node-agent:v[0-9])\")) and
    (.clusterAgent.image.name | test(\"^ghcr[.]io/coroot/coroot-cluster-agent:[0-9]\"))" \
  'the Coroot component images must remain pinned to explicit GHCR tags'

policy='map(select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "observability" and .metadata.name == "allow-coroot"))'

assert "${controllers}" "${policy} | length == 1" \
  'the production overlay must render exactly one observability/allow-coroot policy'

# Registry discovery egress: exact ghcr.io on HTTPS only, inside the FQDN
# allow-list, with no wildcard or world entity widening the policy.
assert "${controllers}" \
  "${policy} | [.[0].spec.egress[] | select(any(.toFQDNs[]?; . == {\"matchName\": \"ghcr.io\"})) | .toPorts] == [[{\"ports\": [{\"port\": \"443\", \"protocol\": \"TCP\"}]}]]" \
  'allow-coroot must permit ghcr.io on TCP 443 only'

assert "${controllers}" \
  "${policy} | [.[0].spec.egress[] | ((.toFQDNs[]? | select(has(\"matchPattern\"))), (.toEntities[]? | select(. == \"world\" or . == \"all\")))] | length == 0" \
  'allow-coroot must not allow wildcard FQDNs or the world/all entities'

printf 'PASS: Coroot agents, project and audit forwarder share one API key; GHCR egress is exact\n'
