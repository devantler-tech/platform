#!/usr/bin/env bash
# Exercise the production placement against the pinned chart and its post-renderers.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/kubescape-placement.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT

# Refuse an incomplete render or unsafe scheduling boundary.
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# A compute-only Linux worker lacks the volume client; only the storage worker is eligible.
safe_placement() {
  jq -e '
    .spec.template.spec.nodeSelector as $selector |
    ($selector | type == "object") and
    ([
      {labels:{"kubernetes.io/os":"linux","node.longhorn.io/create-default-disk":"true"},eligible:true},
      {labels:{"kubernetes.io/os":"linux"},eligible:false},
      {labels:{"kubernetes.io/os":"windows","node.longhorn.io/create-default-disk":"true"},eligible:false}
    ] | all(.[];
      . as $node |
      (all($selector | to_entries[]; $node.labels[.key] == .value)) == $node.eligible))
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
  'version.BuildInfo{Version:"v4.2", GitCommit:"", GitTreeState:"", GoVersion:"go1.26.3", KubeClientVersion:"v1.36"}' ]] ||
  fail 'the audited controller renderer is required'

for mode in install upgrade; do
  args=()
  [[ "$mode" != upgrade ]] || args=(--is-upgrade)
  "$controller_helm" template kubescape "$scratch/kubescape-operator-$version.tgz" \
    --namespace kubescape --kube-version "$kube_version" --values "$scratch/values.json" \
    "${args[@]}" >"$scratch/rendered.yaml"
  yq ea -o=json -I=0 'select(.kind == "Deployment" or .kind == "DaemonSet")' "$scratch/rendered.yaml" |
    jq -s 'map(select(.metadata.name != "storage") |
      {kind,name:.metadata.name,selector:.spec.template.spec.nodeSelector}) | sort_by(.kind,.name)' \
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
  safe_placement "$scratch/storage.json" || fail "$mode storage may select a worker without its volume client"
  yq ea -o=json -I=0 'select(.kind == "Deployment" or .kind == "DaemonSet")' "$scratch/rendered.yaml" |
    jq -s 'map(select(.metadata.name != "storage") |
      {kind,name:.metadata.name,selector:.spec.template.spec.nodeSelector}) | sort_by(.kind,.name)' \
      >"$scratch/after-placement.json"
  cmp -s "$scratch/before-placement.json" "$scratch/after-placement.json" ||
    fail 'placement changed for a component other than storage'
  jq -e '.spec.replicas == 1 and .spec.strategy.type == "Recreate" and
    any(.spec.template.spec.volumes[]; .persistentVolumeClaim.claimName == "kubescape-storage")' \
    "$scratch/storage.json" >/dev/null || fail 'single-writer storage or claim identity changed'
  for mutation in \
    'del(.spec.template.spec.nodeSelector["node.longhorn.io/create-default-disk"])' \
    '.spec.template.spec.nodeSelector["node.longhorn.io/create-default-disk"] = "false"' \
    'del(.spec.template.spec.nodeSelector["kubernetes.io/os"])'; do
    jq "$mutation" "$scratch/storage.json" >"$scratch/mutated.json"
    if safe_placement "$scratch/mutated.json"; then fail "accepted unsafe placement: $mutation"; fi
  done
done
printf 'PASS: production Kubescape storage selects volume-client workers without changing its claim\n'
