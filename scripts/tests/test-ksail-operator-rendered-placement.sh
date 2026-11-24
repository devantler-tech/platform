#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script_dir="${root_dir}/scripts"
die() {
  printf 'UNKNOWN: %s\n' "$1" >&2
  exit 2
}
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}
# shellcheck source=scripts/flux-render.lib.sh
source "${script_dir}/flux-render.lib.sh"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/ksail-operator-placement.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT
flux_render_init "$scratch"
controller_helm="${CONTROLLER_HELM:-}"
flux_render_ensure_renderer

placement_is_scoped() {
  jq -e --slurpfile baseline "$2" '
    def target: .kind == "Deployment" and .metadata.name == "ksail-operator" and .metadata.namespace == "ksail-operator";
    def canonical: map(if target then
      (if .spec.template.spec.affinity.nodeAffinity == {} then del(.spec.template.spec.affinity.nodeAffinity) else . end) |
      (if .spec.template.spec.affinity.podAntiAffinity == {} then del(.spec.template.spec.affinity.podAntiAffinity) else . end) |
      (if .spec.template.spec.affinity == {} then del(.spec.template.spec.affinity) else . end)
      else . end) | sort_by([.apiVersion,.kind,.metadata.namespace,.metadata.name]);
    ([.[] | select(target)]) as $operators |
    ($operators | length) == 1 and
    $operators[0].spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms ==
      [{matchExpressions:[{key:"kubernetes.io/hostname",operator:"NotIn",values:["prod-worker-2","prod-worker-3"]}]}] and
    $operators[0].spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution ==
      [{topologyKey:"kubernetes.io/hostname",labelSelector:{matchLabels:{
        "app.kubernetes.io/component":"operator","app.kubernetes.io/instance":"ksail-operator","app.kubernetes.io/name":"ksail-operator"
      }}}] and
    ((map(if target then del(.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution,
      .spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution) else . end) | canonical) ==
      ($baseline[0] | canonical))
  ' "$1" >/dev/null
}

assert_workload_headroom() {
  jq -e --argjson enabled "$2" --argjson scanner_enabled "$3" '
    def workload($kind;$namespace;$name):
      [.[] | select(.kind == $kind and .metadata.namespace == $namespace and .metadata.name == $name)]
      | if length == 1 then .[0] else error("missing or duplicated workload") end;
    workload("HelmRelease";"kubescape";"kubescape") as $kubescape |
    workload("Deployment";"observability";"crossplane-sync-exporter") as $exporter |
    workload("HelmRelease";"policy-reporter";"policy-reporter") as $reporter |
    $kubescape.spec.values.kubevuln.nodeSelector["kubernetes.io/hostname"] as $scannerHost |
    $kubescape.spec.values.kubevuln.affinity as $scannerAffinity |
    $exporter.spec.template.spec.nodeSelector["kubernetes.io/hostname"] as $exporterHost |
    $reporter.spec.values.ui.affinity.nodeAffinity as $reporterAffinity |
    $scannerHost == null and
    (if $scanner_enabled then
      $scannerAffinity ==
        {nodeAffinity:{requiredDuringSchedulingIgnoredDuringExecution:{nodeSelectorTerms:
          [{matchExpressions:[{key:"kubernetes.io/hostname",operator:"In",values:["prod-worker-1"]}]},
           {matchExpressions:[{key:"storage.platform.devantler.tech/migration-pool",operator:"In",values:["v2"]}]}]}}}
    else
      $scannerAffinity == null
    end) and
    (if $enabled then
      $exporterHost == "prod-worker-1" and $exporter.spec.strategy.type == "Recreate" and
      $reporterAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms ==
        [{matchExpressions:[{key:"kubernetes.io/hostname",operator:"In",values:["prod-worker-1","prod-worker-2"]}]},
         {matchExpressions:[{key:"storage.platform.devantler.tech/migration-pool",operator:"In",values:["v2"]}]}] and
      $reporter.spec.values.ui.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution ==
        [{labelSelector:{matchLabels:{"app.kubernetes.io/name":"policy-reporter-ui"}},topologyKey:"kubernetes.io/hostname"}]
    else
      $exporterHost == null and $reporterAffinity == null
    end)
  ' "$1" >/dev/null || fail 'temporary migration placement is missing, widened or present without the component'
}

kube_version="$(yq -er '.spec.cluster.kubernetesVersion' "${root_dir}/ksail.prod.yaml")"
component='../../../bases/components/longhorn-v1-workload-headroom'
scanner_component='../../../../bases/components/longhorn-v1-runtime-headroom'
for profile in prod local prod-enabled prod-disabled; do
  cluster="$profile"
  tree="${root_dir}/k8s"
  if [[ "$profile" == prod-enabled || "$profile" == prod-disabled ]]; then
    cluster=prod
    mkdir -p "$scratch/$profile"
    cp -R "${root_dir}/k8s" "$scratch/$profile/k8s"
    tree="$scratch/$profile/k8s"
    selection_file="$tree/providers/hetzner/infrastructure/kustomization.yaml"
    scanner_selection_file="$tree/providers/hetzner/infrastructure/controllers/kustomization.yaml"
    if [[ "$profile" == prod-disabled ]]; then
      COMPONENT="$component" yq -i '.components |= map(select(. != strenv(COMPONENT)))' "$selection_file"
      COMPONENT="$scanner_component" yq -i '.components |= map(select(. != strenv(COMPONENT)))' "$scanner_selection_file"
    else
      selected="$(COMPONENT="$component" yq -r '.components | contains([strenv(COMPONENT)])' "$selection_file")"
      case "$selected" in
        true) ;;
        false) COMPONENT="$component" yq -i '.components += [strenv(COMPONENT)]' "$selection_file" ;;
        *) fail 'the explicit workload opt-in is ambiguous' ;;
      esac
      scanner_selected="$(COMPONENT="$scanner_component" yq -r '.components | contains([strenv(COMPONENT)])' "$scanner_selection_file")"
      case "$scanner_selected" in
        true) ;;
        false) COMPONENT="$scanner_component" yq -i '.components += [strenv(COMPONENT)]' "$scanner_selection_file" ;;
        *) fail 'the explicit scanner opt-in is ambiguous' ;;
      esac
    fi
  fi
  enabled=false
  if [[ "$cluster" == prod ]]; then
    enabled="$(COMPONENT="$component" yq -r '.components | contains([strenv(COMPONENT)])' "$tree/providers/hetzner/infrastructure/kustomization.yaml")"
    [[ "$enabled" == true || "$enabled" == false ]] || fail 'the production workload selection is ambiguous'
  fi
  work="$scratch/$profile"
  flux_render_cluster "$tree" "$tree/clusters/$cluster" "$work"
  if [[ "$cluster" == local ]]; then
    # The operator's OIDC-dependent HelmRelease is deliberately prod-only.
    jq -e '[.[] | select(.kind == "HelmRelease" and .metadata.name == "ksail-operator" and .metadata.namespace == "ksail-operator")]
      | length == 0' "$work/resolved.json" >/dev/null || fail 'local inherited the production-only operator release'
    continue
  fi
  scanner_enabled="$(COMPONENT="$scanner_component" yq -r '.components | contains([strenv(COMPONENT)])' "$tree/providers/hetzner/infrastructure/controllers/kustomization.yaml")"
  [[ "$scanner_enabled" == true || "$scanner_enabled" == false ]] || fail 'the production scanner selection is ambiguous'
  assert_workload_headroom "$work/resolved.json" "$enabled" "$scanner_enabled"
  jq -e '[.[] | select(.kind == "HelmRelease" and .metadata.name == "ksail-operator" and .metadata.namespace == "ksail-operator")]
    | if length == 1 then .[0] else error("missing or duplicated operator release") end' "$work/resolved.json" >"$work/release.json"
  jq -e '.spec.valuesFrom == null and .spec.postRenderStrategy == "nohooks" and .spec.chartRef == {kind:"OCIRepository",name:"ksail-operator"}' "$work/release.json" >/dev/null ||
    fail 'the operator renderer value or source contract changed'
  jq -e '[.[] | select(.kind == "OCIRepository" and .metadata.name == "ksail-operator" and .metadata.namespace == "ksail-operator")]
    | if length == 1 then .[0] else error("missing or duplicated operator source") end' "$work/resolved.json" >"$work/source.json"
  jq -e '.spec.url == "oci://ghcr.io/devantler-tech/charts/ksail-operator" and (.spec.ref | keys) == ["tag"] and
    (.spec.ref.tag | test("^[0-9]+[.][0-9]+[.][0-9]+$")) and .spec.verify.provider == "cosign" and
    .spec.layerSelector == {mediaType:"application/vnd.cncf.helm.chart.content.v1.tar+gzip",operation:"copy"}' "$work/source.json" >/dev/null ||
    fail 'the chart must remain bound to its signed reviewed source'
  version="$(jq -r '.spec.ref.tag' "$work/source.json")"
  chart="$scratch/ksail-operator-$version.tgz"
  if [[ ! -f "$chart" ]]; then
    helm pull oci://ghcr.io/devantler-tech/charts/ksail-operator --version "$version" --destination "$scratch" >/dev/null
  fi
  jq '.spec.values // {}' "$work/release.json" >"$work/values.json"
  jq '{release:.}' "$work/release.json" >"$work/record.json"
  if [[ "$enabled" == true ]]; then
    jq 'del(.release.spec.postRenderers[0].kustomize.patches[-1])' "$work/record.json" >"$work/baseline.json"
  else
    cp "$work/record.json" "$work/baseline.json"
  fi
  for mode in install upgrade; do
    set --
    [[ "$mode" != upgrade ]] || set -- --is-upgrade
    HELM_GUARD_CAPTURE="$work/$mode.wire.yaml" "$controller_helm" template ksail-operator "$chart" --namespace ksail-operator \
      --kube-version "$kube_version" --values "$work/values.json" "$@" --post-renderer guard-input-capture >"$work/$mode.yaml"
    for variant in actual baseline; do
      render_dir="$work/$mode.$variant"
      mkdir -p "$render_dir"
      yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' "$work/$mode.wire.yaml" >"$render_dir/resources.yaml"
      record="$work/record.json"
      [[ "$variant" != baseline ]] || record="$work/baseline.json"
      flux_render_apply_post_renderers "$record" "$render_dir" 'KSail operator placement' || fail 'a native post-renderer did not apply'
      yq ea -o=json -I=0 '[.] | map(select(. != null))' "$render_dir/resources.yaml" >"$render_dir.json"
    done
    actual="$work/$mode.actual.json"
    baseline="$work/$mode.baseline.json"
    if [[ "$enabled" == true ]]; then
      placement_is_scoped "$actual" "$baseline" || fail 'the component changed more than required workload placement'
      for mutation in \
        'map(if .kind == "Deployment" then del(.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution) else . end)' \
        'map(if .kind == "Deployment" then .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms += [{}] else . end)' \
        'map(if .kind == "Deployment" then del(.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution) else . end)' \
        '. + [{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"unrelated",namespace:"ksail-operator"}}]'; do
        jq "$mutation" "$actual" >"$work/mutated.json"
        if placement_is_scoped "$work/mutated.json" "$baseline"; then fail 'an ineffective or unrelated change passed'; fi
      done
    else
      jq -e '[.[] | select(.kind == "Deployment" and .metadata.name == "ksail-operator" and .metadata.namespace == "ksail-operator")]
        | length == 1 and .[0].spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution == null and
          .[0].spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution == null' "$actual" >/dev/null ||
        fail 'ordinary placement inherited the temporary constraints'
    fi
  done
done
printf 'PASS: native KSail operator install and upgrade preserve only scoped temporary placement\n'
