#!/usr/bin/env bash
# Exercise the production placement of Kubescape's result store against the pinned chart and its
# post-renderers. Compute-only workers run the Longhorn volume client, so the store follows the
# ordinary rule: any Linux worker, with no pin to the storage-disk workers (#4777). Nothing may
# narrow it again silently, and it keeps its single writer, claim, security settings and probes.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/kubescape-placement.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT

# Refuse an incomplete render or a changed scheduling or safety boundary.
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Every Linux worker is eligible, with or without a storage disk; a Windows node never is,
# and no required node affinity or fixed node name narrows that again.
unpinned_placement() {
  jq -e '
    (.spec.template.spec.nodeName == null) and
    (.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution == null) and
    .spec.template.spec.nodeSelector as $selector |
    ($selector | type == "object") and
    ([
      {labels:{"kubernetes.io/os":"linux","node.longhorn.io/create-default-disk":"true"},eligible:true},
      {labels:{"kubernetes.io/os":"linux"},eligible:true},
      {labels:{"kubernetes.io/os":"windows","node.longhorn.io/create-default-disk":"true"},eligible:false}
    ] | all(.[];
      . as $node |
      (all($selector | to_entries[]; $node.labels[.key] == .value)) == $node.eligible))
  ' "$1" >/dev/null
}

# One writer on its existing claim, the restricted security settings, and both health probes.
store_contract() {
  jq -e '
    .spec.replicas == 1 and .spec.strategy.type == "Recreate" and
    any(.spec.template.spec.volumes[]; .persistentVolumeClaim.claimName == "kubescape-storage") and
    .spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
    (.spec.template.spec.containers | length) == 1 and
    (.spec.template.spec.containers[0] |
      .securityContext.allowPrivilegeEscalation == false and
      .securityContext.runAsNonRoot == true and
      .securityContext.capabilities.drop == ["ALL"] and
      .securityContext.seccompProfile.type == "RuntimeDefault" and
      (.readinessProbe | type) == "object" and (.livenessProbe | type) == "object")
  ' "$1" >/dev/null
}

release="$root/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml"
version="$(yq -er '.spec.chart.spec.version' "$release")"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'the chart must have an exact version'
export HELM_CACHE_HOME="$scratch/cache" HELM_CONFIG_HOME="$scratch/config"
export HELM_DATA_HOME="$scratch/data" HELM_PLUGINS="$scratch/plugins"
helm pull kubescape-operator --repo https://kubescape.github.io/helm-charts/ \
  --version "$version" --destination "$scratch" >/dev/null

kubectl kustomize "$root/k8s/providers/hetzner/infrastructure/controllers" |
  yq ea -o=json -I=0 'select(.kind == "HelmRelease" and .metadata.name == "kubescape")' - |
  jq -e -s 'if length == 1 then .[0] else error("expected one effective release") end' >"$scratch/release.json"
jq '.spec.values | .clusterName = "placement-fixture"' "$scratch/release.json" >"$scratch/values.json"
jq -e '.spec.postRenderers | select(length > 0)' "$scratch/release.json" >"$scratch/renderers.json"
count="$(jq length "$scratch/renderers.json")"
kube_version="$(yq -er '.spec.cluster.kubernetesVersion' "$root/ksail.prod.yaml")"
controller_helm="${CONTROLLER_HELM:-$scratch/controller-helm}"
if [[ -z "${CONTROLLER_HELM:-}" ]]; then
  bash "$root/scripts/build-controller-helm.sh" "$controller_helm"
fi
[[ "$("$controller_helm" version --template '{{ printf "%#v" . }}')" == \
  'version.BuildInfo{Version:"v4.2", GitCommit:"", GitTreeState:"", GoVersion:"go1.26.8", KubeClientVersion:"v1.36"}' ]] ||
  fail 'the audited controller renderer is required'

for mode in install upgrade; do
  args=()
  [[ "$mode" != upgrade ]] || args=(--is-upgrade)
  "$controller_helm" template kubescape "$scratch/kubescape-operator-$version.tgz" \
    --namespace kubescape --kube-version "$kube_version" --values "$scratch/values.json" \
    "${args[@]}" >"$scratch/rendered.yaml"
  # The chart's own placement for every component, the store included.
  yq ea -o=json -I=0 'select(.kind == "Deployment" or .kind == "DaemonSet")' "$scratch/rendered.yaml" |
    jq -s 'map({kind,name:.metadata.name} + (.spec.template.spec | {nodeSelector,affinity,tolerations,nodeName,topologySpreadConstraints})) | sort_by(.kind,.name)' \
      >"$scratch/before-placement.json"
  for ((i=0; i<count; i++)); do
    renderer="$scratch/$mode-renderer-$i"
    mkdir "$renderer"
    cp "$scratch/rendered.yaml" "$renderer/rendered.yaml"
    jq --argjson i "$i" '
      {apiVersion:"kustomize.config.k8s.io/v1beta1",kind:"Kustomization",resources:["rendered.yaml"]} +
      .[$i].kustomize
    ' "$scratch/renderers.json" >"$renderer/kustomization.yaml"
    kubectl kustomize "$renderer" >"$scratch/next.yaml"
    mv "$scratch/next.yaml" "$scratch/rendered.yaml"
  done
  yq ea -o=json -I=0 'select(.kind == "Deployment")' "$scratch/rendered.yaml" |
    jq -e -s 'map(select(.metadata.name == "storage")) |
      if length == 1 then .[0] else error("expected one storage Deployment") end' >"$scratch/storage.json"
  unpinned_placement "$scratch/storage.json" ||
    fail "$mode storage is not eligible for every Linux worker"
  yq ea -o=json -I=0 'select(.kind == "Deployment" or .kind == "DaemonSet")' "$scratch/rendered.yaml" |
    jq -s 'map({kind,name:.metadata.name} + (.spec.template.spec | {nodeSelector,affinity,tolerations,nodeName,topologySpreadConstraints})) | sort_by(.kind,.name)' \
      >"$scratch/after-placement.json"
  cmp -s "$scratch/before-placement.json" "$scratch/after-placement.json" ||
    fail "$mode post-renderers changed a component's placement"
  store_contract "$scratch/storage.json" ||
    fail "$mode storage lost its single writer, claim, security settings or probes"

  # Negative controls: each mutation must be refused by the check it targets.
  for mutation in \
    '.spec.template.spec.nodeSelector["node.longhorn.io/create-default-disk"] = "true"' \
    'del(.spec.template.spec.nodeSelector["kubernetes.io/os"])' \
    'del(.spec.template.spec.nodeSelector)' \
    '.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms = [{"matchExpressions":[{"key":"node.longhorn.io/create-default-disk","operator":"In","values":["true"]}]}]' \
    '.spec.template.spec.nodeName = "storage-worker"'; do
    jq "$mutation" "$scratch/storage.json" >"$scratch/mutated.json"
    if unpinned_placement "$scratch/mutated.json"; then fail "accepted placement: $mutation"; fi
  done
  for mutation in \
    '.spec.replicas = 2' \
    '.spec.strategy.type = "RollingUpdate"' \
    '.spec.template.spec.volumes |= map(select(.persistentVolumeClaim == null))' \
    '.spec.template.spec.securityContext.seccompProfile.type = "Unconfined"' \
    '.spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation = true' \
    '.spec.template.spec.containers[0].securityContext.capabilities.drop = []' \
    'del(.spec.template.spec.containers[0].readinessProbe)' \
    'del(.spec.template.spec.containers[0].livenessProbe)'; do
    jq "$mutation" "$scratch/storage.json" >"$scratch/mutated.json"
    if store_contract "$scratch/mutated.json"; then fail "accepted store change: $mutation"; fi
  done
done
printf 'PASS: production Kubescape storage may run on every Linux worker and keeps its claim and safeguards\n'
