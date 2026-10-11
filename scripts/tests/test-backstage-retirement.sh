#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null || fail 'kubectl is required'
command -v yq >/dev/null || fail 'yq is required'
command -v jq >/dev/null || fail 'jq is required'

for path in k8s/bases/apps/backstage k8s/providers/hetzner/apps/backstage docs/backstage.md; do
  [[ ! -e "${root_dir}/${path}" ]] || fail "retired scaffold remains at ${path}"
done

# Examine every independently reconciled production layer, including embedded
# proxy, authentication and monitoring configuration. A removed app directory
# alone cannot prove that shared layers stopped granting it access.
for layer in bootstrap infrastructure/controllers infrastructure apps; do
  rendered="$(kubectl kustomize "${root_dir}/k8s/providers/hetzner/${layer}")" ||
    fail "production ${layer} must render"
  documents="$(printf '%s\n' "${rendered}" | yq ea -o=json -I=0 '.' -)" ||
    fail "production ${layer} must decode"
  if [[ "${layer}" == apps ]]; then
    # Persistence protection keeps the retired database running until explicit
    # data retention and cleanup. Permit exactly its temporary control and
    # replication policy, never the application or any broader access wiring.
    retained="$(jq -sc '[.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "backstage" and .metadata.name == "allow-retained-backstage-db")]' <<< "${documents}")" ||
      fail 'retained database policy must decode'
    jq -e '
      length == 1 and .[0].apiVersion == "cilium.io/v2" and
      .[0].spec == {
        endpointSelector: {matchLabels: {"cnpg.io/cluster": "backstage-db"}},
        ingress: [{fromEndpoints: [{matchLabels: {"k8s:io.kubernetes.pod.namespace": "backstage", "cnpg.io/cluster": "backstage-db"}}], toPorts: [{ports: [{port: "5432", protocol: "TCP"}]}]}],
        egress: [
          {toEndpoints: [{matchLabels: {"k8s:io.kubernetes.pod.namespace": "backstage", "cnpg.io/cluster": "backstage-db"}}], toPorts: [{ports: [{port: "5432", protocol: "TCP"}]}]},
          {toEntities: ["kube-apiserver"], toPorts: [{ports: [{port: "443", protocol: "TCP"}, {port: "6443", protocol: "TCP"}]}]}
        ]
      }
    ' <<< "${retained}" >/dev/null || fail 'retained database policy is missing or grants unexpected access'
    documents="$(jq -c 'select(.kind != "CiliumNetworkPolicy" or .metadata.namespace != "backstage" or .metadata.name != "allow-retained-backstage-db")' <<< "${documents}")" ||
      fail 'remaining production apps must decode'
  fi
  if grep -qi backstage <<< "${documents}"; then
    fail "production ${layer} still deploys or references the retired portal"
  fi
done

printf 'PASS: retired portal access is absent; retained database has only temporary control and replication paths\n'
