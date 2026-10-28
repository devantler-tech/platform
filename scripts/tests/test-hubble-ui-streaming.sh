#!/usr/bin/env bash
set -euo pipefail

mode="${1:-}"
case "$mode" in --render-only|--runtime) ;; *) printf 'usage: %s --render-only|--runtime\n' "$0" >&2; exit 2 ;; esac
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
helm_bin="${CONTROLLER_HELM:?use the audited controller Helm renderer}"
pull_bin="${HELM_PULL_BIN:-helm}"
release="$root_dir/k8s/bases/infrastructure/controllers/cilium/helm-release.yaml"
chart_version="$(yq -er '.spec.chart.spec.version' "$release")"
chart_repo="$(yq -er '.spec.url' "$root_dir/k8s/bases/infrastructure/controllers/cilium/helm-repository.yaml")"
kube_version="$(yq -er '.spec.cluster.kubernetesVersion' "$root_dir/ksail.prod.yaml")"
"$pull_bin" pull cilium --repo "$chart_repo" --version "$chart_version" --destination "$scratch" >/dev/null
if [[ "$mode" == --runtime ]]; then
  command -v docker >/dev/null || fail 'the runtime regression requires Docker; use --render-only for limited local proof'
  (cd "$root_dir/scripts/check-hubble-ui-streaming" && go build -mod=readonly -o "$scratch/stream-probe" .)
fi
normalize() { awk '{sub(/^[ \t]+/, "");sub(/[ \t]+$/, "");if(length) print}' "$1"; }
runtime_key=''
for provider in hetzner docker; do
  fixture="$scratch/$provider"
  mkdir -p "$fixture/post"
  kubectl kustomize "$root_dir/k8s/providers/$provider/infrastructure/controllers" |
    yq ea 'select(.kind == "HelmRelease" and .metadata.name == "cilium")' - >"$fixture/release.yaml"
  [[ "$(yq -er '.metadata.name' "$fixture/release.yaml")" == cilium ]] || fail "$provider has no Cilium release"
  # Exercise the declared UI values in a one-replica chart fixture. This changes
  # no consumer's replica count and needs no cluster credentials or discovery.
  yq '{"hubble":{"enabled":.spec.values.hubble.enabled,"relay":{"enabled":.spec.values.hubble.relay.enabled},"ui":.spec.values.hubble.ui}}' "$fixture/release.yaml" >"$fixture/values.yaml"
  yq -i '.hubble.ui.replicas = 1' "$fixture/values.yaml"
  "$helm_bin" template cilium "$scratch/cilium-$chart_version.tgz" --namespace kube-system \
    --kube-version "$kube_version" --values "$fixture/values.yaml" \
    --show-only templates/hubble-ui/configmap.yaml --show-only templates/hubble-ui/deployment.yaml >"$fixture/rendered.yaml"
  yq ea 'select(.kind == "ConfigMap" and .metadata.name == "hubble-ui-nginx")' "$fixture/rendered.yaml" >"$fixture/post/baseline.yaml"
  yq -er '.data["nginx.conf"]' "$fixture/post/baseline.yaml" >"$fixture/baseline.conf"
  yq -o=json '.spec.postRenderers // []' "$fixture/release.yaml" |
    jq '[.[] | .kustomize.patches[]?]' >"$fixture/patches.json"
  printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - baseline.yaml\n' >"$fixture/post/kustomization.yaml"
  PATCHES="$fixture/patches.json" yq -i '.patches = load(strenv(PATCHES))' "$fixture/post/kustomization.yaml"
  kubectl kustomize "$fixture/post" >"$fixture/patched.yaml"
  yq -er '.data["nginx.conf"]' "$fixture/patched.yaml" >"$fixture/candidate.conf"
  normalize "$fixture/baseline.conf" | awk '{print; if ($0 == "proxy_http_version 1.1;") print "proxy_buffering off;"}' >"$fixture/expected.conf"
  normalize "$fixture/candidate.conf" >"$fixture/normalized.conf"
  cmp -s "$fixture/expected.conf" "$fixture/normalized.conf" ||
    fail "$provider must change the actual chart API configuration only by disabling response buffering"
  [[ "$(grep -Fc 'proxy_buffering off;' "$fixture/normalized.conf")" == 1 ]] || fail 'exactly one API buffering directive is required'
  config_digest="$(sha256sum "$fixture/normalized.conf" | awk '{print $1}')"
  rollout_digest="$(yq ea -er 'select(.kind == "Deployment" and .metadata.name == "hubble-ui") | .spec.template.metadata.annotations["platform.devantler.tech/hubble-ui-nginx-sha256"]' "$fixture/rendered.yaml")"
  [[ "$rollout_digest" == "$config_digest" ]] || fail 'the UI pod checksum must identify the normalized effective proxy configuration'
  [[ "$(yq -er '.spec.postRenderStrategy' "$fixture/release.yaml")" == nohooks ]] || fail 'post-rendering must retain the audited nohooks strategy'
  image="$(yq ea -er 'select(.kind == "Deployment" and .metadata.name == "hubble-ui") | .spec.template.spec.containers[] | select(.name == "frontend") | .image' "$fixture/rendered.yaml")"
  [[ "$image" =~ ^quay\.io/cilium/hubble-ui:[^@]+@sha256:[0-9a-f]{64}$ ]] || fail 'the actual frontend image must retain its immutable upstream digest'
  if [[ "$mode" == --runtime ]]; then
    key="$(sha256sum "$fixture/candidate.conf" | awk '{print $1}'):$image"
    if [[ "$key" != "$runtime_key" ]]; then
      "$scratch/stream-probe" --image "$image" --baseline "$fixture/baseline.conf" --candidate "$fixture/candidate.conf"
      runtime_key="$key"
    fi
  fi
  printf 'PASS: %s pinned chart UI configuration preserves routing and enables API streaming\n' "$provider"
done
[[ "$mode" != --render-only ]] || printf 'LIMITATION: frontend runtime behavior was not exercised; CI must run --runtime\n'
