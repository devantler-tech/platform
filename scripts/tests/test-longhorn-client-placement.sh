#!/usr/bin/env bash
# Exercise the pinned chart's volume-client placement and separate disk gate.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
release_dir="${root}/k8s/providers/hetzner/infrastructure/controllers/longhorn"
work="$(mktemp -d)"
completed=0
cleanup() {
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: client-placement test aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
for tool in kubectl helm yq jq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done
[[ "$(helm version --template '{{.Version}}')" == v4.2.4 ]] || fail 'the audited Helm v4.2.4 chart puller is required'
[[ -n "${CONTROLLER_HELM:-}" && -x "${CONTROLLER_HELM}" ]] || fail 'the audited controller SDK renderer is required'
[[ "$("${CONTROLLER_HELM}" version --template '{{ printf "%#v" . }}')" == \
  'version.BuildInfo{Version:"v4.2", GitCommit:"", GitTreeState:"", GoVersion:"go1.26.8", KubeClientVersion:"v1.36"}' ]] ||
  fail 'controller SDK renderer metadata does not match Flux 2.9.6'
kube_version="$(yq -er '.spec.cluster.kubernetesVersion | select(tag == "!!str" and . != "")' "${root}/ksail.prod.yaml")"

kubectl kustomize "${release_dir}" | yq -o=json \
  'select(.kind == "HelmRelease" and .metadata.name == "longhorn")' >"${work}/release.json"
jq -e '.spec.chart.spec.version == "1.13.0" and
  .spec.values.defaultSettings.createDefaultDiskLabeledNodes == true and
  (.spec.values.defaultSettings | has("createDefaultDiskOnLabeledNodesOnly") | not) and
  .spec.values.defaultSettings.systemManagedComponentsNodeSelector == "" and
  .spec.values.longhornManager.nodeSelector == {} and
  (.spec.values.global.nodeSelector // {}) == {} and
  (.spec.values.longhornDriver.nodeSelector // {}) == {}' "${work}/release.json" >/dev/null ||
  fail 'clients must not inherit the replica-storage selector, and the real chart disk gate must be enabled'

yq -o=json '.data' "${root}/k8s/clusters/prod/bootstrap/config-map.yaml" >"${work}/variables.json"
jq --slurpfile variables "${work}/variables.json" '.spec.values | walk(
  if type == "string" and test("^\\$\\{[a-z_]+:=[0-9]+\\}$") then
    capture("^\\$\\{(?<name>[a-z_]+):=(?<fallback>[0-9]+)\\}$") |
    ($variables[0][.name] // .fallback) | tonumber
  else . end)' "${work}/release.json" >"${work}/fixed-values.json"
# Reproduce the shipping mistake without changing the checked-out manifest.
jq '.defaultSettings |= (del(.createDefaultDiskLabeledNodes) |
  .createDefaultDiskOnLabeledNodesOnly = true |
  .systemManagedComponentsNodeSelector = "node.longhorn.io/create-default-disk:true") |
  .longhornManager.nodeSelector = {"node.longhorn.io/create-default-disk":"true"}' \
  "${work}/fixed-values.json" >"${work}/legacy-values.json"
chart_repo="$(yq -r '.spec.url' "${release_dir}/helm-repository.yaml")"
helm pull longhorn --repo "${chart_repo}" --version 1.13.0 --destination "${work}"

render() {
  local mode="$1" phase="$2" index count
  local dir="${work}/${mode}-${phase}"
  mkdir "${dir}"
  local args=()
  [[ "${phase}" == install ]] || args+=(--is-upgrade)
  "${CONTROLLER_HELM}" template longhorn "${work}/longhorn-1.13.0.tgz" --namespace longhorn-system \
    --kube-version "${kube_version}" --values "${work}/${mode}-values.json" "${args[@]}" >"${dir}/chart.yaml"
  # Releases set postRenderStrategy: nohooks, so lifecycle hooks stay untouched.
  yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' \
    "${dir}/chart.yaml" >"${dir}/resources.yaml"
  yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] != null)' \
    "${dir}/chart.yaml" >"${dir}/hooks.yaml"
  count="$(jq '.spec.postRenderers | length' "${work}/release.json")"
  for ((index = 0; index < count; index++)); do
    jq --argjson index "${index}" '{apiVersion:"kustomize.config.k8s.io/v1beta1",
      kind:"Kustomization",resources:["resources.yaml"]} +
      .spec.postRenderers[$index].kustomize' "${work}/release.json" >"${dir}/kustomization.yaml"
    kubectl kustomize "${dir}" >"${dir}/next.yaml"
    mv "${dir}/next.yaml" "${dir}/resources.yaml"
  done
  yq ea -o=json '[.]' "${dir}/resources.yaml" "${dir}/hooks.yaml" |
    jq -S 'sort_by([.apiVersion,.kind,.metadata.namespace,.metadata.name])' >"${dir}/resources.json"
  yq 'select(.kind == "ConfigMap" and .metadata.name == "longhorn-default-setting") |
    .data."default-setting.yaml" | from_yaml' "${dir}/resources.yaml" |
    yq -o=json >"${dir}/settings.json"
}

clients_eligible() {
  local dir="$1"
  jq -e '[.[] | select((.kind == "DaemonSet" and .metadata.name == "longhorn-manager") or
      (.kind == "Deployment" and .metadata.name == "longhorn-driver-deployer")) |
    (.spec.template.spec.nodeSelector // {}) == {}] == [true,true]' "${dir}/resources.json" >/dev/null &&
    jq -e '."create-default-disk-labeled-nodes" == true and
      ."system-managed-components-node-selector" == ""' "${dir}/settings.json" >/dev/null
}

hooks_eligible() {
  jq -e '[.[] | select(.kind == "Job" and
    (.metadata.name == "longhorn-pre-upgrade" or .metadata.name == "longhorn-post-upgrade" or
     .metadata.name == "longhorn-uninstall")) |
    (.spec.template.spec.nodeSelector // {}) == {}] == [true,true,true]' "$1" >/dev/null
}

for phase in install upgrade; do
  render fixed "${phase}"
  render legacy "${phase}"
  clients_eligible "${work}/fixed-${phase}" || fail "${phase}: chart-rendered clients or disk gate is wrong"
  hooks_eligible "${work}/fixed-${phase}/resources.json" || fail "${phase}: manager lifecycle hooks retain the storage selector"
  if clients_eligible "${work}/legacy-${phase}"; then
    fail "${phase}: negative control accepted the legacy selector and ignored chart setting"
  fi
  # The manager lifecycle hooks inherit this same selector in the pinned chart.
  # Every other chart resource and pod property must be identical, including
  # permissions, resource reservations, CSI settings, UI hardening and storage.
  for mode in fixed legacy; do
    jq -S 'map(if (.kind == "DaemonSet" and .metadata.name == "longhorn-manager") or
        (.kind == "Job" and (.metadata.name == "longhorn-pre-upgrade" or
          .metadata.name == "longhorn-post-upgrade" or .metadata.name == "longhorn-uninstall")) then
        del(.spec.template.spec.nodeSelector)
      elif .kind == "ConfigMap" and .metadata.name == "longhorn-default-setting" then
        del(.data."default-setting.yaml")
      else . end)' "${work}/${mode}-${phase}/resources.json" >"${work}/${mode}-normalized.json"
    jq -S 'del(."create-default-disk-labeled-nodes", ."system-managed-components-node-selector")' \
      "${work}/${mode}-${phase}/settings.json" >"${work}/${mode}-settings.json"
  done
  if ! cmp "${work}/fixed-normalized.json" "${work}/legacy-normalized.json"; then
    diff -u "${work}/legacy-normalized.json" "${work}/fixed-normalized.json" || true
    fail "${phase}: unrelated chart resources changed"
  fi
  cmp "${work}/fixed-settings.json" "${work}/legacy-settings.json" ||
    fail "${phase}: unrelated Longhorn settings changed"
done

# The separate, fail-closed compute-node replica policy remains deployed.
policy="${root}/k8s/providers/hetzner/infrastructure/cluster-policies/restrict-storage-to-baseline-workers.yaml"
yq -o=json '.' "${policy}" | jq -e '(.spec.failurePolicy // "Fail") == "Fail" and
  .spec.admission != false and
  [.spec.rules[] | select(.name == "disable-replica-scheduling-on-autoscale-nodes") |
    .mutate.patchStrategicMerge.spec.allowScheduling == false and
    .mutate.patchStrategicMerge.spec.evictionRequested == true] == [true]' >/dev/null ||
  fail 'compute workers must remain excluded from replica storage'
grep -Fq 'cluster-policies/restrict-storage-to-baseline-workers.yaml' \
  "${root}/k8s/providers/hetzner/infrastructure/kustomization.yaml" || fail 'replica exclusion must remain deployed'

completed=1
printf 'PASS: pinned install/upgrade renders enable volume clients, preserve replica exclusion and change no unrelated chart behavior\n'
