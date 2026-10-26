#!/usr/bin/env bash
# Inspect the real production overlay. Runtime policy realization remains separate.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

kubectl kustomize "$root/k8s/providers/hetzner/apps" > "$scratch/production.yaml"
yq -o=json -I=0 '.' "$scratch/production.yaml" | jq -s '.' > "$scratch/production.json"
check_hold() {
  jq -e '
    [ .[] | select(.kind == "CiliumNetworkPolicy" and .metadata.name == "business-site-staging-hold") ] |
    length == 1 and .[0].metadata.namespace == "business-site" and
    .[0].spec == {
      endpointSelector: {matchLabels: {"app.kubernetes.io/name": "business-site"}},
      ingressDeny: [{fromEntities: ["ingress"], toPorts: [{ports: [{port: "8080", protocol: "TCP"}]}]}]
    }
  ' "$1" >/dev/null
}
check_hold "$scratch/production.json" || { echo 'missing or broadened staging ingress hold' >&2; exit 1; }
# These controls prevent an empty selector, a changed port/direction, or a mere
# additive allow policy from satisfying the independently enforced hold.
for change in \
  'map(select(.metadata.name != "business-site-staging-hold"))' \
  'map(if .metadata.name == "business-site-staging-hold" then .spec.endpointSelector={} else . end)' \
  'map(if .metadata.name == "business-site-staging-hold" then .metadata.namespace="another-app" else . end)' \
  'map(if .metadata.name == "business-site-staging-hold" then .spec.ingress=.spec.ingressDeny | del(.spec.ingressDeny) else . end)' \
  'map(if .metadata.name == "business-site-staging-hold" then .spec.ingressDeny[0].fromEntities=["all"] else . end)' \
  'map(if .metadata.name == "business-site-staging-hold" then .spec.ingressDeny[0].toPorts[0].ports[0].port="80" else . end)' \
  'map(if .metadata.name == "business-site-staging-hold" then .spec.egress=[{}] else . end)'; do
  jq "$change" "$scratch/production.json" > "$scratch/changed.json"
  if check_hold "$scratch/changed.json"; then echo 'unsafe hold mutation was accepted' >&2; exit 1; fi
done
# Installing the hold is deliberately independent of enabling the application.
jq -e '
  [.[] | select(.kind == "Tenant" and .metadata.name == "business-site")] |
  length == 1 and .[0].metadata.namespace == "flux-system" and
  .[0].spec.webApp == false and .[0].spec.externalDns == false and
  .[0].spec.source.reconcileApplication == false
' "$scratch/production.json" >/dev/null
kubectl kustomize "$root/k8s/providers/docker/apps" > "$scratch/local.yaml"
yq -o=json -I=0 '.' "$scratch/local.yaml" | jq -es 'all(.[]; .metadata.name != "business-site-staging-hold")' >/dev/null
printf 'PASS: production-only staging hold; seven unsafe mutations refused; application remains disabled\n'
