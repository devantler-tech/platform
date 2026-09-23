#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly alertmanager_release="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/alertmanager/helm-release.yaml"
readonly backstage_release="${root_dir}/k8s/bases/apps/backstage/helm-release.yaml"
readonly loadtester_release="${root_dir}/k8s/bases/infrastructure/controllers/flagger/helm-release-loadtester.yaml"
readonly loadtester_pdb="${root_dir}/k8s/bases/infrastructure/controllers/flagger/pod-disruption-budget-loadtester.yaml"
readonly kubescape_alert_route="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kubescape/patches/route-runtime-detection-alerts.yaml"
readonly crossplane_alerter="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cron-job-crossplane-sync-alerter.yaml"
readonly dr_runbook="${root_dir}/docs/dr/velero-cnpg.md"
readonly longhorn_release="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml"
readonly origin_ca_release="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer/helm-release.yaml"
readonly cluster_issuers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/cluster-issuers"
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
command -v kubectl >/dev/null || fail 'kubectl is required'

yq e -e '
  .spec.values.backstage.startupProbe.httpGet.path == "/.backstage/health/v1/readiness" and
  .spec.values.backstage.startupProbe.failureThreshold == 30
' "${backstage_release}" >/dev/null ||
  fail 'Backstage must retry startup if backend initialization never reaches readiness'

yq e -e '
  .spec.values.replicaCount == 2 and
  .spec.values.podDisruptionBudget.enabled == false and
  ([.spec.values.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[] |
    select(.topologyKey == "kubernetes.io/hostname" and
      .labelSelector.matchLabels.app == "loadtester")
  ] | length == 1)
' "${loadtester_release}" >/dev/null ||
  fail 'Flagger loadtester must have two cross-node replicas and a drain-safe PDB'

yq e -e '
  .kind == "PodDisruptionBudget" and
  .metadata.name == "flagger-loadtester" and
  .metadata.namespace == "flagger-system" and
  .spec.maxUnavailable == 1 and
  (.spec | has("minAvailable") | not) and
  .spec.selector.matchLabels.app == "loadtester" and
  .spec.selector.matchLabels."app.kubernetes.io/name" == "loadtester" and
  (.spec.selector.matchLabels | has("app.kubernetes.io/instance") | not)
' "${loadtester_pdb}" >/dev/null ||
  fail 'Flagger loadtester must use a platform-owned maxUnavailable PDB'

loadtester_patch="$(yq e -r '.spec.postRenderers[].kustomize.patches[] | select(.target.kind == "Deployment" and .target.name == "flagger-loadtester") | .patch' "${loadtester_release}")"
printf '%s\n' "${loadtester_patch}" | yq e -e '
  .spec.strategy.type == "RollingUpdate" and
  .spec.strategy.rollingUpdate.maxUnavailable == 1 and
  .spec.strategy.rollingUpdate.maxSurge == 0
' - >/dev/null ||
  fail 'Flagger loadtester rollout must not deadlock on two eligible workers'

yq e -e '
  .spec.values.replicaCount == 2 and
  .spec.values.podAntiAffinity == "hard" and
  .spec.values.podDisruptionBudget.maxUnavailable == 1
' "${alertmanager_release}" >/dev/null ||
  fail 'Alertmanager must run two cross-node peers behind a drain-safe PDB'

# Alertmanager HA does not replicate received alerts, so both producers must
# post to each peer rather than to the load-balanced Service.
yq e -e '
  (.spec.values.nodeAgent.config.alertManagerExporterUrls | join(",")) ==
    "alertmanager-0.alertmanager-headless.kubescape.svc:9093,alertmanager-1.alertmanager-headless.kubescape.svc:9093"
' "${kubescape_alert_route}" >/dev/null ||
  fail 'the node-agent must export runtime alerts to every Alertmanager peer'

grep -Fq 'AM_PEERS="http://alertmanager-0.alertmanager-headless.kubescape.svc.cluster.local.:9093 http://alertmanager-1.alertmanager-headless.kubescape.svc.cluster.local.:9093"' \
  "${crossplane_alerter}" ||
  fail 'the Crossplane sync alerter must post to every Alertmanager peer'
if grep -Fq 'alertmanager.kubescape.svc.cluster.local:9093' "${crossplane_alerter}"; then
  fail 'the Crossplane sync alerter must not post through the load-balanced Service'
fi

grep -Fq 'longhorn_csi_snapshotter_replicas: "2"' "${dr_runbook}" ||
  fail 'the Velero/CNPG runbook must document the warm-standby snapshotter value'

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

cluster_issuers_render="$(kubectl kustomize "${cluster_issuers_dir}")" ||
  fail 'the production ClusterIssuer layer must render'
if printf '%s\n' "${cluster_issuers_render}" | yq e -e '
  select(.kind == "ClusterOriginIssuer" and .metadata.name == "cloudflare-origin")
' - >/dev/null 2>&1; then
  fail 'the disabled Origin CA controller must not leave a fresh ClusterOriginIssuer waiting for status'
fi

printf 'PASS: actionable Coroot availability risks are encoded as safe HA or an effective scale-to-zero\n'
