#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl > /dev/null || fail 'kubectl is required'
command -v yq > /dev/null || fail 'yq is required'
command -v jq > /dev/null || fail 'jq is required'

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
  if [[ "${layer}" == infrastructure ]]; then
    # This names the retired app solely to protect native cleanup and retained
    # data. It is not app deployment, routing, credential or access wiring.
    fence="$(jq -sc '[.[] | select(.kind == "ClusterPolicy" and .metadata.name == "restrict-backstage-retirement")]' <<< "${documents}")" ||
      fail 'retirement admission must decode'
    jq -e 'length==1 and .[0].spec.background==false and
      .[0].spec.webhookConfiguration.failurePolicy=="Fail" and
      (.[0].spec.rules|length)==5 and
      all(.[0].spec.rules[]; .validate.failureAction=="Enforce" and .validate.allowExistingViolations==false)' <<< "${fence}" > /dev/null ||
      fail 'exact enforced retirement admission is missing'
    documents="$(jq -c 'select(.kind != "ClusterPolicy" or .metadata.name != "restrict-backstage-retirement")' <<< "${documents}")" ||
      fail 'remaining production infrastructure must decode'
  fi
  if [[ "${layer}" == apps ]]; then
    # Persistence protection keeps the retired database running until explicit
    # data retention and cleanup. Permit exactly its temporary control and
    # replication/monitoring policy and protected namespace, never the application
    # or broader access wiring. The namespace must also exist on a fresh rebuild.
    retained_namespace="$(jq -sc '[.[] | select(.kind == "Namespace" and .metadata.name == "backstage")]' <<< "${documents}")" ||
      fail 'retained database namespace must decode'
    jq -e '
      length == 1 and .[0].apiVersion == "v1" and
      .[0].metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "disabled" and
      .[0].metadata.labels["pod-security.kubernetes.io/enforce"] == "restricted"
    ' <<< "${retained_namespace}" > /dev/null || fail 'retained database namespace is missing or unprotected'
    retained="$(jq -sc '[.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "backstage" and .metadata.name == "allow-retained-backstage-db")]' <<< "${documents}")" ||
      fail 'retained database policy must decode'
    jq -e '
      length == 1 and .[0].apiVersion == "cilium.io/v2" and
      .[0].spec == {
        endpointSelector: {matchLabels: {"cnpg.io/cluster": "backstage-db"}},
        ingress: [
          {fromEndpoints: [{matchLabels: {"k8s:io.kubernetes.pod.namespace": "backstage", "cnpg.io/cluster": "backstage-db"}}], toPorts: [{ports: [{port: "5432", protocol: "TCP"}]}]},
          {fromEndpoints: [{matchLabels: {"k8s:io.kubernetes.pod.namespace": "observability", "app.kubernetes.io/managed-by": "coroot-operator", "app.kubernetes.io/part-of": "coroot", "app.kubernetes.io/component": "coroot-cluster-agent"}}], toPorts: [{ports: [{port: "5432", protocol: "TCP"}]}]}
        ],
        egress: [
          {toEndpoints: [{matchLabels: {"k8s:io.kubernetes.pod.namespace": "backstage", "cnpg.io/cluster": "backstage-db"}}], toPorts: [{ports: [{port: "5432", protocol: "TCP"}]}]},
          {toEntities: ["kube-apiserver"], toPorts: [{ports: [{port: "443", protocol: "TCP"}, {port: "6443", protocol: "TCP"}]}]}
        ]
      }
    ' <<< "${retained}" > /dev/null || fail 'retained database policy is missing or grants unexpected access'
    monitor="$(jq -sc '[.[] | select(.kind == "ExternalSecret" and .metadata.namespace == "backstage" and .metadata.name == "backstage-db-coroot-monitor")]' <<< "${documents}")" || fail 'monitoring dependency must decode'
    jq -e 'length == 1 and .[0].apiVersion == "external-secrets.io/v1" and .[0].spec == {
      refreshPolicy: "CreatedOnce",
      target: {name: "backstage-db-coroot-monitor", creationPolicy: "Owner", template: {
        type: "kubernetes.io/basic-auth", metadata: {labels: {"cnpg.io/reload": "true"}},
        data: {username: "coroot_monitor", password: "{{ .password }}"}
      }},
      dataFrom: [{sourceRef: {generatorRef: {apiVersion: "generators.external-secrets.io/v1alpha1", kind: "Password", name: "backstage-db-coroot-monitor"}}}]
    }' <<< "${monitor}" > /dev/null || fail 'retained database monitoring dependency is missing or periodically regenerated'
    generator="$(jq -sc '[.[] | select(.kind == "Password" and .metadata.namespace == "backstage" and .metadata.name == "backstage-db-coroot-monitor")]' <<< "${documents}")" || fail 'monitoring generator must decode'
    jq -e 'length == 1 and .[0].apiVersion == "generators.external-secrets.io/v1alpha1" and .[0].spec == {
      length: 32, digits: 8, symbols: 0, noUpper: false, allowRepeat: true
    }' <<< "${generator}" > /dev/null || fail 'dedicated monitoring generator is missing'
    documents="$(jq -c 'select((.kind != "CiliumNetworkPolicy" or .metadata.namespace != "backstage" or .metadata.name != "allow-retained-backstage-db") and (.kind != "Namespace" or .metadata.name != "backstage") and ((.kind != "ExternalSecret" and .kind != "Password") or .metadata.namespace != "backstage" or .metadata.name != "backstage-db-coroot-monitor"))' <<< "${documents}")" ||
      fail 'remaining production apps must decode'
  fi
  if grep -qi backstage <<< "${documents}"; then
    fail "production ${layer} still deploys or references the retired portal"
  fi
done

printf 'PASS: retired portal access is absent; retained database control, replication and dedicated monitoring are preserved\n'
