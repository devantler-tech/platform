#!/usr/bin/env bash
# Inspect the real production overlay. Runtime policy realization remains separate.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

kubectl kustomize "$root/k8s/providers/hetzner/apps" >"$scratch/production.yaml"
yq -o=json -I=0 '.' "$scratch/production.yaml" | jq -s '.' >"$scratch/production.json"
# The staging ingress hold was lifted once the signed application ran healthy
# behind it (devantler-tech/business-site#554). Its name stays reserved by
# restrict-tenant-network-policies, so only the platform can put it back.
jq -e 'all(.[]; .kind != "CiliumNetworkPolicy" or .metadata.name != "business-site-staging-hold")' \
  "$scratch/production.json" >/dev/null || {
  echo 'the lifted staging ingress hold is still rendered' >&2
  exit 1
}
check_identity_hold() {
  jq -e '
    [.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.name == "business-site-customer-identity-hold")] |
    length == 1 and .[0].metadata.namespace == "business-site" and
    .[0].spec == {
      endpointSelector: {matchExpressions: [{
        key: "app.kubernetes.io/name", operator: "In", values: ["customer-dex", "customer-zitadel"]
      }]},
      enableDefaultDeny: {ingress: false},
      ingressDeny: [{fromEntities: ["ingress"]}]
    }
  ' "$1" >/dev/null
}
check_identity_hold "$scratch/production.json" || {
  echo 'missing or broadened customer identity ingress hold' >&2
  exit 1
}
for change in \
  'empty' \
  '.metadata.namespace="another-app"' \
  '.spec.endpointSelector={}' \
  '.spec.endpointSelector.matchExpressions[0].values=["customer-dex"]' \
  '.spec.endpointSelector.matchExpressions[0].values += ["business-site"]' \
  'del(.spec.enableDefaultDeny)' \
  '.spec.ingress=.spec.ingressDeny | del(.spec.ingressDeny)' \
  '.spec.ingressDeny[0].fromEntities=["world"]' \
  '.spec.ingressDeny[0].toPorts=[{ports:[{port:"5556",protocol:"TCP"}]}]'; do
  jq "map(if .metadata.name == \"business-site-customer-identity-hold\" then ($change) else . end)" \
    "$scratch/production.json" >"$scratch/changed.json"
  if check_identity_hold "$scratch/changed.json"; then
    echo 'unsafe customer identity hold mutation was accepted' >&2
    exit 1
  fi
done
# The public staging application reconciles and receives gateway traffic; the
# customer identity hold above still keeps the portal's identity provider closed.
jq -e '
  [.[] | select(.kind == "Tenant" and .metadata.name == "business-site")] |
  length == 1 and .[0].metadata.namespace == "flux-system" and
  .[0].spec.webApp == true and .[0].spec.externalDns == false and
  .[0].spec.source.reconcileApplication == true
' "$scratch/production.json" >/dev/null
kubectl kustomize "$root/k8s/providers/docker/apps" >"$scratch/local.yaml"
yq -o=json -I=0 '.' "$scratch/local.yaml" |
  jq -es 'all(.[]; .metadata.name != "business-site-staging-hold" and .metadata.name != "business-site-customer-identity-hold")' >/dev/null
printf 'PASS: staging hold lifted; production-only identity hold; 9 unsafe mutations refused; application reconciles\n'
