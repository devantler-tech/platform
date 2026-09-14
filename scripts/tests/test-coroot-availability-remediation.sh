#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly alertmanager_release="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/alertmanager/helm-release.yaml"
readonly longhorn_release="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml"
readonly origin_ca_release="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/helm-release.yaml"
readonly prod_variables="${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml"
readonly replica_floor="${root_dir}/k8s/bases/infrastructure/cluster-policies/best-practices/validate-replica-floor.yaml"
# These are intentionally literal Flux post-build substitution expressions.
# shellcheck disable=SC2016
readonly snapshotter_substitution='${longhorn_csi_snapshotter_replicas:=1}'
# shellcheck disable=SC2016
readonly origin_ca_substitution='${origin_ca_issuer_replicas:=0}'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null || fail 'yq is required'

yq e -e '
  .spec.values.replicaCount == 2 and
  .spec.values.podAntiAffinity == "hard" and
  .spec.values.podDisruptionBudget.maxUnavailable == 1
' "${alertmanager_release}" >/dev/null ||
  fail 'Alertmanager must run two cross-node peers behind a drain-safe PDB'

yq e -e '.data.longhorn_csi_snapshotter_replicas == "2"' \
  "${prod_variables}" >/dev/null ||
  fail 'production must run a warm-standby Longhorn CSI snapshotter'

SNAPSHOTTER_SUBSTITUTION="${snapshotter_substitution}" yq e -e \
  '.spec.values.csi.snapshotterReplicaCount == strenv(SNAPSHOTTER_SUBSTITUTION)' \
  "${longhorn_release}" >/dev/null ||
  fail 'Longhorn must continue sourcing the snapshotter replica count from the production contract'

if grep -Fxq '              - csi-snapshotter' "${replica_floor}"; then
  fail 'the HA CSI snapshotter must not remain exempt from the replica floor'
fi

ORIGIN_CA_SUBSTITUTION="${origin_ca_substitution}" yq e -e '
  .spec.values.controller.replicaCount == strenv(ORIGIN_CA_SUBSTITUTION) and
  (.spec.values | has("replicaCount") | not)
' "${origin_ca_release}" >/dev/null ||
  fail 'Origin CA Issuer must use the chart-supported controller.replicaCount scale-to-zero value'

printf 'PASS: actionable Coroot availability risks are encoded as safe HA or an effective scale-to-zero\n'
