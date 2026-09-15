#!/usr/bin/env bash

# Exercise the pinned Crossview chart through the production Flux
# post-renderers. A HelmRelease-only assertion cannot prove that the database
# Deployment consumes the intended PVC or that its single-writer rollout is
# safe.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
scratch_dir="$(mktemp -d)"
readonly scratch_dir
trap 'rm -rf "${scratch_dir}"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

for tool in helm jq kubectl yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

render_overlay() {
  local overlay="$1"
  local destination="$2"

  kubectl kustomize "${overlay}" |
    yq ea -o=json '[.]' |
    jq -S 'sort_by([.apiVersion, .kind, .metadata.namespace, .metadata.name])' \
      >"${destination}"
}

extract_release() {
  local resources="$1"
  local destination="$2"

  jq -e '.[] | select(
    .kind == "HelmRelease" and
    .metadata.namespace == "crossview" and
    .metadata.name == "crossview"
  )' "${resources}" >"${destination}"
}

render_overlay "${root_dir}/k8s/providers/hetzner/apps" "${scratch_dir}/prod.json"
render_overlay "${root_dir}/k8s/providers/docker/apps" "${scratch_dir}/local.json"
render_overlay "${root_dir}/k8s/bases/apps/crossview" "${scratch_dir}/base.json"
extract_release "${scratch_dir}/prod.json" "${scratch_dir}/prod-release.json"
extract_release "${scratch_dir}/base.json" "${scratch_dir}/base-release.json"

jq -e '
  (.spec.values.database.persistence.enabled == true) and
  (.spec.values.database.persistence.size == "2Gi") and
  (.spec.values.database.persistence.accessMode == "ReadWriteOnce") and
  (.spec.values.database.persistence.storageClass == "longhorn-wffc") and
  (.metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "disabled")
' "${scratch_dir}/prod-release.json" >/dev/null ||
  fail 'production Crossview PostgreSQL must declare a protected 2Gi longhorn-wffc RWO volume'

jq -e '
  (.spec.values.database.persistence.enabled == false) and
  (.spec.values.database.persistence.storageClass // null) == null
' "${scratch_dir}/base-release.json" >/dev/null ||
  fail 'the reusable Crossview base must keep persistence disabled without a Longhorn dependency'

jq -e '[.[] | select(
  .kind == "HelmRelease" and
  .metadata.namespace == "crossview" and
  .metadata.name == "crossview"
)] | length == 0' "${scratch_dir}/local.json" >/dev/null ||
  fail 'the default Docker provider must remain free of the production-only Crossview release'

jq -e '[.[] | select(
  .kind == "Namespace" and .metadata.name == "crossview"
) | (
  (.metadata.labels["pod-security.devantler.tech/user-namespaces"] // null) == null and
  .metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "disabled"
)] == [true]' "${scratch_dir}/prod.json" >/dev/null ||
  fail 'the mixed Crossview namespace must leave userns enforcement disabled while PostgreSQL uses Longhorn'

chart_name="$(jq -r '.spec.chart.spec.chart' "${scratch_dir}/prod-release.json")"
chart_version="$(jq -r '.spec.chart.spec.version' "${scratch_dir}/prod-release.json")"
chart_repo="$(yq -r '.spec.url' "${root_dir}/k8s/bases/apps/crossview/helm-repository.yaml")"
helm pull "${chart_name}" --repo "${chart_repo}" --version "${chart_version}" \
  --destination "${scratch_dir}"
readonly chart_archive="${scratch_dir}/${chart_name}-${chart_version}.tgz"

# Resolve the numeric production substitutions that Helm must consume. Other
# substitutions are string-valued URLs or credentials and are safe to preserve
# literally for this chart render.
yq -o=json '.data' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml" \
  >"${scratch_dir}/variables.json"
jq --slurpfile variables "${scratch_dir}/variables.json" '.spec.values | walk(
  if type == "string" and test("^\\$\\{[a-z_]+:=[0-9]+\\}$") then
    capture("^\\$\\{(?<name>[a-z_]+):=(?<fallback>[0-9]+)\\}$") |
    ($variables[0][.name] // .fallback) | tonumber
  else . end
)' "${scratch_dir}/prod-release.json" >"${scratch_dir}/values.json"

helm template crossview "${chart_archive}" --namespace crossview \
  --values "${scratch_dir}/values.json" >"${scratch_dir}/resources.yaml"

renderer_count="$(jq '.spec.postRenderers | length' "${scratch_dir}/prod-release.json")"
for ((renderer_index = 0; renderer_index < renderer_count; renderer_index++)); do
  render_dir="${scratch_dir}/renderer-${renderer_index}"
  mkdir "${render_dir}"
  cp "${scratch_dir}/resources.yaml" "${render_dir}/resources.yaml"
  jq --argjson index "${renderer_index}" '{
    apiVersion: "kustomize.config.k8s.io/v1beta1",
    kind: "Kustomization",
    resources: ["resources.yaml"]
  } + .spec.postRenderers[$index].kustomize' \
    "${scratch_dir}/prod-release.json" >"${render_dir}/kustomization.yaml"
  kubectl kustomize "${render_dir}" >"${scratch_dir}/next.yaml"
  mv "${scratch_dir}/next.yaml" "${scratch_dir}/resources.yaml"
done

yq ea -o=json '[.]' "${scratch_dir}/resources.yaml" |
  jq -S 'sort_by([.apiVersion, .kind, .metadata.namespace, .metadata.name])' \
    >"${scratch_dir}/workload.json"

jq -e '[.[] | select(
  .kind == "PersistentVolumeClaim" and
  .metadata.namespace == "crossview" and
  .metadata.name == "crossview-postgres-pvc"
) | (
  .spec.storageClassName == "longhorn-wffc" and
  .spec.accessModes == ["ReadWriteOnce"] and
  .spec.resources.requests.storage == "2Gi" and
  .metadata.annotations["helm.sh/resource-policy"] == "keep"
)] == [true]' "${scratch_dir}/workload.json" >/dev/null ||
  fail 'the pinned chart must render the exact retained Crossview PostgreSQL PVC contract'

jq -e '[.[] | select(
  .kind == "ConfigMap" and
  .metadata.namespace == "crossview" and
  .metadata.name == "crossview-postgres-coroot-monitor-init"
) | .data["init-coroot-monitor.sh"] | contains("durable-storage-v1")] == [true]' \
  "${scratch_dir}/prod.json" >/dev/null ||
  fail 'the fresh persistent database must create the durable-storage bootstrap gate'

database_filter='select(
  .kind == "Deployment" and
  .metadata.namespace == "crossview" and
  .metadata.name == "crossview-postgres"
)'

jq -e "[.[] | ${database_filter}] | length == 1" \
  "${scratch_dir}/workload.json" >/dev/null ||
  fail 'the pinned chart must render exactly one Crossview PostgreSQL Deployment'

jq -e "[.[] | ${database_filter} | (
  .spec.replicas == 1 and
  .spec.strategy.type == \"RollingUpdate\" and
  .spec.strategy.rollingUpdate.maxSurge == 0 and
  .spec.strategy.rollingUpdate.maxUnavailable == 1 and
  .spec.template.metadata.annotations[\"platform.devantler.tech/durability-proof\"] ==
    \"2026-09-15-pod-restart-v1\"
)] == [true]" "${scratch_dir}/workload.json" >/dev/null ||
  fail 'the single-writer PostgreSQL rollout must carry the reviewed database-only restart proof'

jq -e "[.[] | ${database_filter} |
  .spec.template.spec.hostUsers == true
] == [true]" "${scratch_dir}/workload.json" >/dev/null ||
  fail 'only the PVC-backed PostgreSQL Deployment must explicitly retain host users'

jq -e "[.[] | ${database_filter} | (
  ([.spec.template.spec.volumes[] | select(
    .name == \"postgres-storage\" and
    .persistentVolumeClaim.claimName == \"crossview-postgres-pvc\"
  )] | length == 1) and
  ([.spec.template.spec.volumes[] | select(.name == \"postgres-data\")] | length == 0)
)] == [true]" "${scratch_dir}/workload.json" >/dev/null ||
  fail 'the database pod must consume only the chart-owned persistent data volume'

jq -e "[.[] | ${database_filter} | (
  ([.spec.template.spec.containers[] | select(.name == \"postgres\") |
    .volumeMounts[] | select(
      .name == \"postgres-storage\" and .mountPath == \"/var/lib/postgresql\"
    )] | length == 1) and
  ([.spec.template.spec.containers[] | select(.name == \"postgres\") |
    .volumeMounts[] | select(.mountPath == \"/var/lib/postgresql\")
  ] | length == 1)
)] == [true]" "${scratch_dir}/workload.json" >/dev/null || {
  jq -c "[.[] | ${database_filter} |
    .spec.template.spec.containers[] | select(.name == \"postgres\") |
    .volumeMounts
  ]" "${scratch_dir}/workload.json" >&2
  fail 'the database container must mount exactly one persistent data directory'
}

jq -e '[.[] | select(
  .kind == "Deployment" and
  .metadata.namespace == "crossview" and
  .metadata.name == "crossview"
) | (
  (.spec.template.spec.hostUsers // null) == null and
  .spec.template.metadata.annotations["platform.devantler.tech/db-bootstrap"] ==
    "2026-09-15-durable-storage-v1" and
  ([.spec.template.spec.initContainers[] | select(.name == "wait-for-db") |
    .args[] | contains("durable-storage-v1")
  ] | any)
)] == [true]' \
  "${scratch_dir}/workload.json" >/dev/null ||
  fail 'the Crossview app must restart only after the durable database bootstrap gate exists'

printf 'PASS: Crossview PostgreSQL renders durable single-writer storage without changing the local provider\n'
