#!/usr/bin/env bash
# OIDC discovery must not hold the layer that precedes its Gateway configuration.
# Render the actual provider layers: source-file placement alone is insufficient.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

render() {
  kubectl kustomize "${root}/k8s/$1" |
    yq -o=json -I=0 '.' | jq -s 'map(select(type == "object"))' >"$2"
}

for provider in docker hetzner; do
  render "providers/${provider}/infrastructure/controllers" "${work}/${provider}-early.json"
  render "providers/${provider}/infrastructure" "${work}/${provider}-late.json"
  jq -e '
    [.[] | select(.kind == "HelmRelease" and .metadata.name == "ksail-operator")] | length == 0
  ' "${work}/${provider}-early.json" >/dev/null ||
    fail "${provider}: KSail still gates the layer before Gateway recovery"
done

jq -e '
  . as $early |
  ["Namespace", "OCIRepository", "HTTPRoute", "CiliumNetworkPolicy", "PodDisruptionBudget"] |
  all(.[]; . as $kind |
    [$early[] | select(.kind == $kind and
      .metadata.name == (if $kind == "CiliumNetworkPolicy" then "allow-ksail-operator" else "ksail-operator" end) and
      (if $kind == "Namespace" then (.metadata.namespace // "") == ""
       else .metadata.namespace == "ksail-operator" end))] | length == 1)
' "${work}/hetzner-early.json" >/dev/null || fail 'production: an early KSail prerequisite is missing or duplicated'

jq -e '
  [.[] | select(.kind == "OCIRepository" and .metadata.name == "ksail-operator")] |
  length == 1 and .[0].spec.verify.provider == "cosign" and
  (.[0].spec.verify.matchOIDCIdentity | length > 0)
' "${work}/hetzner-early.json" >/dev/null || fail 'production: the early signed chart source is missing'

jq -e '
  [.[] | select(.kind == "HelmRelease" and .metadata.name == "ksail-operator")] |
  length == 1 and .[0].metadata.namespace == "ksail-operator" and
  .[0].metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "disabled" and
  .[0].spec.chartRef == {kind: "OCIRepository", name: "ksail-operator"} and
  .[0].spec.values.auth.oidc == {
    enabled: true, issuerURL: "https://dex.${domain}", clientID: "public-client",
    clientSecret: "${dex_client_secret}", redirectURL: "https://ksail.${domain}/api/v1/auth/callback",
    scopes: "openid email profile groups"
  } and
  .[0].spec.driftDetection.mode == "enabled" and
  .[0].spec.install.remediation.retries == -1 and
  .[0].spec.upgrade.remediation.retries == -1
' "${work}/hetzner-late.json" >/dev/null || fail 'production: the retained late release or its authentication/defaults changed'

jq -e '
  [.[] | select(.kind == "Gateway" and .metadata.name == "platform" and .metadata.namespace == "kube-system")] |
  length == 1
' "${work}/hetzner-late.json" >/dev/null || fail 'production: Gateway is missing from the release layer'

# Local KSail remains opt-in: moving the production release must not enable it.
jq -e '
  [.[] | select(.kind == "HelmRelease" and .metadata.name == "ksail-operator")] | length == 0
' "${work}/docker-late.json" >/dev/null || fail 'Docker unexpectedly enables KSail'

for cluster in local prod; do
  render "clusters/${cluster}" "${work}/${cluster}.json"
  jq -e '
    [.[] | select(.kind == "Kustomization" and .metadata.name == "infrastructure-controllers")] as $early |
    [.[] | select(.kind == "Kustomization" and .metadata.name == "infrastructure")] as $late |
    ($early | length) == 1 and ($late | length) == 1 and
    $early[0].spec.wait == true and $late[0].spec.wait == true and
    $early[0].spec.prune == true and $late[0].spec.prune == true and
    $early[0].spec.force == false and $late[0].spec.force == false and
    $early[0].spec.sourceRef == $late[0].spec.sourceRef and
    $late[0].spec.path == ($early[0].spec.path | rtrimstr("/controllers")) and
    any($late[0].spec.dependsOn[]; .name == "infrastructure-controllers")
  ' "${work}/${cluster}.json" >/dev/null || fail "${cluster}: ordered wait/prune gates changed"
done

echo 'PASS: KSail follows early prerequisites and shares Gateway recovery without changing local opt-in or wait gates'
