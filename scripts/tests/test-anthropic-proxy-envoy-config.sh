#!/usr/bin/env bash
# Prove the Agentic Engineer lane's Anthropic proxy starts on the Envoy it ships with.
#
# The proxy's bootstrap reads its bearer token through a file-based SDS secret. Without a node
# identity, Envoy rejects that config at startup, so the proxy crash-looped and failed the merge
# group's prod deploy (devantler-tech/platform#5905). This validates the ConfigMap's bootstrap
# with the Deployment's own pinned Envoy image, against a placeholder token in the shape the
# token refresher writes, and checks that dropping the node identity fails the same validation.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="${root}/k8s/providers/hetzner/apps/agentic-engineer"
work="$(mktemp -d)"
completed=0
# Remove the work directory on exit, and fail any exit that did not reach the final PASS.
cleanup() {
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: anthropic-proxy envoy config: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: anthropic-proxy envoy config: %s\n' "$1" >&2
  exit 1
}
for tool in docker yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

image="$(yq '.spec.template.spec.containers[] | select(.name == "envoy") | .image' "${dir}/deployment-anthropic-proxy.yaml")"
[[ "${image}" == docker.io/envoyproxy/envoy:*@sha256:* ]] || fail "unexpected envoy image: ${image}"
# shellcheck disable=SC2016 # $a is a yq variable, not a shell one.
config_path="$(yq '.spec.template.spec.containers[] | select(.name == "envoy") | .args as $a |
  $a[($a | to_entries | map(select(.value == "--config-path")) | .[0].key) + 1]' "${dir}/deployment-anthropic-proxy.yaml")"
[[ "${config_path}" == /etc/anthropic-proxy/envoy.yaml ]] || fail "unexpected config path: ${config_path}"

mkdir -p "${work}/good" "${work}/bad" "${work}/sds"
yq '.data["envoy.yaml"]' "${dir}/config-map-anthropic-proxy.yaml" >"${work}/good/envoy.yaml"
yq 'del(.node)' "${work}/good/envoy.yaml" >"${work}/bad/envoy.yaml"
grep -q 'path: /sds/anthropic.yaml' "${work}/good/envoy.yaml" || fail 'the bootstrap no longer reads the SDS token file'
# The refresher's secret shape, with a placeholder in place of the token.
printf 'resources:\n- "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret\n  name: anthropic-authorization\n  generic_secret:\n    secret:\n      inline_string: "Bearer placeholder"\n' >"${work}/sds/anthropic.yaml"
grep -Fq 'name: anthropic-authorization\n  generic_secret:' "${dir}/config-map-anthropic-proxy.yaml" ||
  fail 'the token refresher no longer writes the secret shape this test stands in for'
chmod -R a+rX "${work}"

# validate <bootstrap dir>: runs Envoy's own config validation, prints its output.
validate() {
  docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
    --mount "type=bind,src=$1,dst=/etc/anthropic-proxy,readonly" \
    --mount "type=bind,src=${work}/sds,dst=/sds,readonly" \
    "${image}" --mode validate --config-path "${config_path}" 2>&1
}

out="$(validate "${work}/good")" || fail "Envoy rejects the shipped bootstrap: $(tail -c 600 <<<"${out}")"
grep -q "configuration '${config_path}' OK" <<<"${out}" || fail "Envoy did not confirm the bootstrap: $(tail -c 600 <<<"${out}")"

# Negative control: the same bootstrap without its node identity is today's crash.
if out="$(validate "${work}/bad")"; then
  fail 'validation accepts a bootstrap without a node identity, so it cannot catch the startup crash'
fi
grep -q "node 'id' and 'cluster' are required" <<<"${out}" ||
  fail "the node-less bootstrap failed for another reason: $(tail -c 600 <<<"${out}")"

grep -Fq 'bash scripts/tests/test-anthropic-proxy-envoy-config.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'anthropic-proxy envoy config: PASS\n'
