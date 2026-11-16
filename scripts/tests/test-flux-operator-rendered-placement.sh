#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script_dir="${root_dir}/scripts"
die() { printf 'UNKNOWN: %s\n' "$1" >&2; exit 2; }
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
# shellcheck source=scripts/flux-render.lib.sh
source "${script_dir}/flux-render.lib.sh"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/flux-operator-placement.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT
flux_render_init "$scratch"
controller_helm="${CONTROLLER_HELM:-}"
flux_render_ensure_renderer

# Compare the complete native render. The one permitted difference is the
# additional required hostname expression on the operator Deployment.
placement_is_scoped() {
  jq -e --slurpfile baseline "$2" '
    def canonical: sort_by([.apiVersion,.kind,.metadata.namespace,.metadata.name]);
    ([.[] | select(.kind == "Deployment" and .metadata.name == "flux-operator" and .metadata.namespace == "flux-system")]) as $operators |
    ($operators | length) == 1 and
    $operators[0].spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms ==
      [{matchExpressions:[{key:"kubernetes.io/os",operator:"In",values:["linux"]},
        {key:"kubernetes.io/hostname",operator:"NotIn",values:["prod-worker-3"]}]}] and
    ((map(if .kind == "Deployment" and .metadata.name == "flux-operator" and .metadata.namespace == "flux-system"
      then .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions |=
        map(select(.key != "kubernetes.io/hostname")) else . end) | canonical) == ($baseline[0] | canonical))
  ' "$1" >/dev/null
}

kube_version="$(yq -er '.spec.cluster.kubernetesVersion' "${root_dir}/ksail.prod.yaml")"
for profile in prod local prod-disabled; do
  cluster="$profile"
  tree="${root_dir}/k8s"
  if [[ "$profile" == prod-disabled ]]; then
    cluster=prod
    mkdir -p "$scratch/disabled"
    cp -R "${root_dir}/k8s" "$scratch/disabled/k8s"
    tree="$scratch/disabled/k8s"
    yq -i '.components |= map(select(. != "../../../../bases/components/longhorn-v1-runtime-headroom"))' \
      "$tree/providers/hetzner/infrastructure/controllers/kustomization.yaml"
  fi
  operator_placement_enabled=false
  if [[ "$cluster" == prod ]]; then
    selected="$(yq -r '.components | contains(["../../../../bases/components/longhorn-v1-runtime-headroom"])' \
      "$tree/providers/hetzner/infrastructure/controllers/kustomization.yaml")" || fail 'cannot classify the production headroom component'
    case "$selected" in
      true) operator_placement_enabled=true ;;
      false) ;;
      *) fail 'the production headroom selection is ambiguous' ;;
    esac
  fi
  work="${scratch}/${profile}"
  flux_render_cluster "$tree" "$tree/clusters/${cluster}" "$work"
  jq -e '[.[] | select(.kind == "HelmRelease" and .metadata.name == "flux-operator" and .metadata.namespace == "flux-system")]
    | if length == 1 then .[0] else error("missing or duplicated operator release") end' "$work/resolved.json" >"$work/release.json"
  jq -e '.spec.valuesFrom == null and .spec.postRenderStrategy == "nohooks" and
    .spec.chartRef == {kind:"OCIRepository",name:"flux-operator"}' "$work/release.json" >/dev/null ||
    fail 'requalify the operator renderer if its value or source contract changes'
  jq -e '[.[] | select(.kind == "OCIRepository" and .metadata.name == "flux-operator" and .metadata.namespace == "flux-system")]
    | if length == 1 then .[0] else error("missing or duplicated operator source") end' "$work/resolved.json" >"$work/source.json"
  jq -e '.spec.url == "oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator" and
    (.spec.ref | keys) == ["tag"] and (.spec.ref.tag | test("^[0-9]+[.][0-9]+[.][0-9]+$")) and
    .spec.layerSelector == {mediaType:"application/vnd.cncf.helm.chart.content.v1.tar+gzip",operation:"copy"}' "$work/source.json" >/dev/null ||
    fail 'the operator chart must remain bound to its reviewed source'
  version="$(jq -r '.spec.ref.tag' "$work/source.json")"
  chart="${scratch}/flux-operator-${version}.tgz"
  if [[ ! -f "$chart" ]]; then
    helm pull oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator --version "$version" --destination "$scratch" >/dev/null
  fi
  jq '.spec.values // {}' "$work/release.json" >"$work/values.json"
  jq '{release:.}' "$work/release.json" >"$work/record.json"
  if [[ "$operator_placement_enabled" == true ]]; then
    # The production component appends exactly one placement renderer patch.
    jq 'del(.release.spec.postRenderers[0].kustomize.patches[-1])' "$work/record.json" >"$work/baseline.json"
  else
    cp "$work/record.json" "$work/baseline.json"
  fi
  for mode in install upgrade; do
    set --
    [[ "$mode" != upgrade ]] || set -- --is-upgrade
    HELM_GUARD_CAPTURE="$work/$mode.wire.yaml" "$controller_helm" template flux-operator "$chart" --namespace flux-system \
      --kube-version "$kube_version" --values "$work/values.json" "$@" --post-renderer guard-input-capture >"$work/$mode.yaml"
    for variant in actual baseline; do
      render_dir="$work/$mode.$variant"
      mkdir -p "$render_dir"
      yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' "$work/$mode.wire.yaml" >"$render_dir/resources.yaml"
      record="$work/record.json"
      [[ "$variant" != baseline ]] || record="$work/baseline.json"
      flux_render_apply_post_renderers "$record" "$render_dir" 'Flux operator placement' || fail 'a native operator post-renderer did not apply'
      yq ea -o=json -I=0 '[.] | map(select(. != null))' "$render_dir/resources.yaml" >"$render_dir.json"
    done
    actual="$work/$mode.actual.json"
    baseline="$work/$mode.baseline.json"
    if [[ "$operator_placement_enabled" == true ]]; then
      placement_is_scoped "$actual" "$baseline" || fail 'production must change only the required operator hostname exclusion'
      for mutation in \
        'map(if .kind == "Deployment" then del(.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution) else . end)' \
        'map(if .kind == "Deployment" then .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms += [{matchExpressions:[{key:"kubernetes.io/os",operator:"In",values:["linux"]}]}] else . end)' \
        'map(if .kind == "Deployment" then .metadata.name = "wrong-target" else . end)' \
        '. + [{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"unrelated",namespace:"flux-system"}}]'; do
        jq "$mutation" "$actual" >"$work/mutated.json"
        if placement_is_scoped "$work/mutated.json" "$baseline"; then
          fail 'an ineffective or unrelated rendered change passed the placement guard'
        fi
      done
    else
      jq -e '[.[] | select(.kind == "Deployment" and .metadata.name == "flux-operator" and .metadata.namespace == "flux-system")] as $operators |
        ($operators | length) == 1 and $operators[0].spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms ==
        [{matchExpressions:[{key:"kubernetes.io/os",operator:"In",values:["linux"]}]}]' "$actual" >/dev/null ||
        fail 'an ordinary operator profile inherited temporary migration placement or lost its Deployment'
    fi
  done
done
printf 'PASS: native operator install and upgrade preserve scoped placement, disabled production and ordinary Docker profiles\n'
