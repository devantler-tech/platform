#!/usr/bin/env bash

# Actual Budget runs in its own user namespace in production (#3604, permanent
# since #3605). Exercise the deployed chart and every Flux post-renderer: a
# HelmRelease-only assertion cannot detect a chart value that never reaches its
# pod template.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
scratch_dir="$(mktemp -d)"
readonly scratch_dir
trap 'rm -rf "${scratch_dir}"' EXIT

# Print the supplied failure message to stderr and stop the regression.
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

for tool in kubectl yq jq helm; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

cp -R "${root_dir}/k8s" "${scratch_dir}/k8s"
readonly apps_dir="${scratch_dir}/k8s/providers/hetzner/apps"
# The two patch entries that together make up the setting: the namespace label
# for the admission rule and the post-renderer that sets hostUsers.
readonly label_patch='actual-budget/patches/enable-user-namespaces.yaml'
readonly pod_patch='actual-budget/patches/disable-host-users.yaml'

# Render the overlay directory in $1 to a sorted JSON resource array at $2.
render_overlay() {
  kubectl kustomize "$1" | yq ea -o=json '[.]' | jq -S . >"$2"
}

# Write the Actual Budget HelmRelease from resource array $1 to file $2.
# Fail when the overlay does not contain the expected release.
extract_release() {
  jq -e '.[] | select(.kind == "HelmRelease" and
    .metadata.namespace == "actual-budget" and .metadata.name == "actual-budget")' "$1" >"$2"
}

# Production must reference each patch exactly once; the pod patch must target
# exactly the Actual Budget HelmRelease.
LABEL="${label_patch}" yq -e '[.patches[] | select(.path == strenv(LABEL))] | length == 1' \
  "${apps_dir}/kustomization.yaml" >/dev/null 2>&1 ||
  fail 'production must reference the Actual Budget namespace label patch exactly once'
POD="${pod_patch}" yq -o=json '[.patches[] | select(.path == strenv(POD))]' "${apps_dir}/kustomization.yaml" 2>/dev/null |
  jq -e 'length == 1 and .[0].target == {"group": "helm.toolkit.fluxcd.io", "version": "v2",
    "kind": "HelmRelease", "name": "actual-budget", "namespace": "actual-budget"}' >/dev/null ||
  fail 'production must reference the Actual Budget hostUsers patch exactly once, targeting only its HelmRelease'
# Keep both entries so re-enabling after the rollback below restores them as committed.
LABEL="${label_patch}" POD="${pod_patch}" yq -o=json \
  '[.patches[] | select(.path == strenv(LABEL) or .path == strenv(POD))]' \
  "${apps_dir}/kustomization.yaml" >"${scratch_dir}/entries.json"

render_overlay "${apps_dir}" "${scratch_dir}/on.json"
render_overlay "${scratch_dir}/k8s/providers/docker/apps" "${scratch_dir}/local-before.json"
# The setting is production-only: the local overlay must carry neither the
# namespace enforcement label nor a post-renderer that sets hostUsers.
jq -e '([.[] | select(.kind == "Namespace" and .metadata.name == "actual-budget") |
    .metadata.labels["pod-security.devantler.tech/user-namespaces"] | select(. != null)] | length == 0) and
  ([.[] | select(.kind == "HelmRelease" and .metadata.namespace == "actual-budget" and
      .metadata.name == "actual-budget") | .spec.postRenderers[]? | tostring |
    select(test("hostUsers"))] | length == 0)' \
  "${scratch_dir}/local-before.json" >/dev/null || fail 'the setting must not reach the local overlay'
extract_release "${scratch_dir}/on.json" "${scratch_dir}/on-release.json"
jq -e '[.[] | select(.kind == "Namespace" and .metadata.name == "actual-budget") |
  .metadata.labels["pod-security.devantler.tech/user-namespaces"]] == ["enabled"]' \
  "${scratch_dir}/on.json" >/dev/null || fail 'production must enable the namespace enforcement policy'

# Build the disabled state by removing both patch entries, which is exactly
# the documented rollback.
LABEL="${label_patch}" POD="${pod_patch}" \
  yq -i '.patches |= map(select(.path != strenv(LABEL) and .path != strenv(POD)))' \
  "${apps_dir}/kustomization.yaml"
render_overlay "${apps_dir}" "${scratch_dir}/off.json"
extract_release "${scratch_dir}/off.json" "${scratch_dir}/off-release.json"
jq -e '[.[] | select(.kind == "Namespace" and .metadata.name == "actual-budget") |
  .metadata.labels["pod-security.devantler.tech/user-namespaces"]] == [null]' \
  "${scratch_dir}/off.json" >/dev/null || fail 'rollback must remove the namespace enforcement label'

# The complete overlay may differ only by this namespace label and one appended
# post-renderer on this release. This catches collateral changes to other apps,
# storage declarations, security contexts, or existing authentication patches.
jq -S 'map(if .kind == "Namespace" and .metadata.name == "actual-budget" then
    del(.metadata.labels["pod-security.devantler.tech/user-namespaces"])
  elif .kind == "HelmRelease" and .metadata.namespace == "actual-budget" and
      .metadata.name == "actual-budget" then
    .spec.postRenderers |= .[:-1]
  else . end)' "${scratch_dir}/on.json" >"${scratch_dir}/normalized.json"
diff -u "${scratch_dir}/off.json" "${scratch_dir}/normalized.json" ||
  fail 'the setting changes resources beyond the pod and namespace user-namespace settings'

render_overlay "${scratch_dir}/k8s/providers/docker/apps" "${scratch_dir}/local-after.json"
cmp "${scratch_dir}/local-before.json" "${scratch_dir}/local-after.json" ||
  fail 'the production setting must not affect the local overlay'

# Re-enabling after a rollback must reproduce the committed render exactly.
ENTRIES="$(cat "${scratch_dir}/entries.json")" yq -i '.patches += (strenv(ENTRIES) | from_json)' \
  "${apps_dir}/kustomization.yaml"
render_overlay "${apps_dir}" "${scratch_dir}/reenabled.json"
cmp "${scratch_dir}/on.json" "${scratch_dir}/reenabled.json" ||
  fail 're-enabling after rollback must restore the committed resources exactly'

chart_name="$(jq -r '.spec.chart.spec.chart' "${scratch_dir}/off-release.json")"
chart_version="$(jq -r '.spec.chart.spec.version' "${scratch_dir}/off-release.json")"
chart_repo="$(yq -r '.spec.url' "${root_dir}/k8s/bases/apps/actual-budget/helm-repository.yaml")"
helm pull "${chart_name}" --repo "${chart_repo}" --version "${chart_version}" \
  --destination "${scratch_dir}"
readonly chart_archive="${scratch_dir}/${chart_name}-${chart_version}.tgz"

# Render the off/on release named by $1 with its production replica count and
# ordered Flux post-renderers, saving the resulting workload as sorted JSON.
render_workload() {
  local mode="$1" renderer_count index replicas configured_replicas
  local release="${scratch_dir}/${mode}-release.json"
  local render_dir="${scratch_dir}/${mode}"
  mkdir -p "${render_dir}"
  # Resolve the declared default and production ConfigMap instead of forcing
  # one replica: the assertion below must catch an unsafe production increase.
  replicas="$(jq -r '.spec.values.replicaCount' "${release}")"
  if [[ "${replicas}" =~ ^\$\{actual_budget_replicas:=([0-9]+)\}$ ]]; then
    replicas="${BASH_REMATCH[1]}"
    configured_replicas="$(yq -r '.data.actual_budget_replicas // ""' \
      "${scratch_dir}/k8s/clusters/prod/bootstrap/config-map.yaml")"
    replicas="${configured_replicas:-${replicas}}"
  fi
  [[ "${replicas}" =~ ^[0-9]+$ ]] || fail 'production replica count must resolve to an integer'
  jq --arg replicas "${replicas}" '.spec.values | .replicaCount = ($replicas | tonumber)' \
    "${release}" >"${render_dir}/values.json"
  helm template actual-budget "${chart_archive}" --namespace actual-budget \
    --values "${render_dir}/values.json" >"${render_dir}/resources.yaml"
  renderer_count="$(jq '.spec.postRenderers | length' "${release}")"
  for ((index = 0; index < renderer_count; index++)); do
    jq --argjson index "${index}" '{apiVersion: "kustomize.config.k8s.io/v1beta1",
      kind: "Kustomization", resources: ["resources.yaml"]} +
      .spec.postRenderers[$index].kustomize' "${release}" >"${render_dir}/kustomization.yaml"
    kubectl kustomize "${render_dir}" >"${render_dir}/next.yaml"
    mv "${render_dir}/next.yaml" "${render_dir}/resources.yaml"
  done
  yq ea -o=json '[.]' "${render_dir}/resources.yaml" | jq -S . >"${scratch_dir}/${mode}-workload.json"
}

render_workload off
render_workload on
jq -e '[.[] | select(.kind == "Deployment") | .spec.template.spec.hostUsers] == [false]' \
  "${scratch_dir}/on-workload.json" >/dev/null || fail 'production must reach the rendered Deployment'
jq -e '[.[] | select(.kind == "Deployment") | .spec.template.spec.hostUsers] == [null]' \
  "${scratch_dir}/off-workload.json" >/dev/null || fail 'rollback must restore host user namespaces'
jq -S 'map(if .kind == "Deployment" then del(.spec.template.spec.hostUsers) else . end)' \
  "${scratch_dir}/on-workload.json" >"${scratch_dir}/normalized-workload.json"
diff -u "${scratch_dir}/off-workload.json" "${scratch_dir}/normalized-workload.json" ||
  fail 'the setting must preserve the complete workload, including PVCs and both containers'
jq -e '[.[] | select(.kind == "Deployment") |
  .spec.strategy.type == "Recreate" and .spec.replicas == 1 and
  (.spec.template.spec.containers | length) == 2 and
  ([.spec.template.spec.volumes[] | select(has("persistentVolumeClaim"))] | length) == 1] == [true]' \
  "${scratch_dir}/on-workload.json" >/dev/null || fail 'the setting must retain its single-writer storage contract'

printf 'PASS: Actual Budget runs in its own user namespace in production; the setting changes only hostUsers and enforcement, and rollback preserves storage\n'
