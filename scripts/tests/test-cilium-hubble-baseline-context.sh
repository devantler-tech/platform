#!/usr/bin/env bash
# Render the pinned Cilium chart with the HelmRelease values. The Hubble relay and UI
# must carry the two non-privilege C-0211 defaults (#3239), and removing them must
# restore the chart's render exactly, so the values change nothing else.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
release_dir="${root_dir}/k8s/bases/infrastructure/controllers/cilium"

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
for tool in helm yq jq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

yq -o=json '.' "${release_dir}/helm-release.yaml" >"${scratch}/release.json"
yq -o=json '.data' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml" >"${scratch}/variables.json"
jq --slurpfile variables "${scratch}/variables.json" '.spec.values | walk(
  if type == "string" and test("^\\$\\{[a-z_]+:=[0-9]+\\}$") then
    capture("^\\$\\{(?<name>[a-z_]+):=(?<fallback>[0-9]+)\\}$") |
    ($variables[0][.name] // .fallback) | tonumber
  else . end)' "${scratch}/release.json" >"${scratch}/on-values.json"
# The disabled state removes only the two defaults this test is about.
jq 'del(.hubble.relay.podSecurityContext.fsGroupChangePolicy) |
  del(.hubble.relay.securityContext.seLinuxOptions) |
  del(.hubble.ui.securityContext.fsGroupChangePolicy) |
  del(.hubble.ui.frontend.securityContext.seLinuxOptions) |
  del(.hubble.ui.backend.securityContext.seLinuxOptions)' \
  "${scratch}/on-values.json" >"${scratch}/off-values.json"

chart_name="$(jq -r '.spec.chart.spec.chart' "${scratch}/release.json")"
chart_version="$(jq -r '.spec.chart.spec.version' "${scratch}/release.json")"
chart_repo="$(yq -r '.spec.url' "${release_dir}/helm-repository.yaml")"
helm pull "${chart_name}" --repo "${chart_repo}" --version "${chart_version}" --destination "${scratch}"

render() {
  local mode="$1"
  helm template cilium "${scratch}/${chart_name}-${chart_version}.tgz" \
    --namespace kube-system --values "${scratch}/${mode}-values.json" >"${scratch}/${mode}.yaml"
  yq ea -o=json '[.]' "${scratch}/${mode}.yaml" |
    jq -S 'map(select(. != null)) | sort_by([.apiVersion,.kind,.metadata.namespace,.metadata.name])' \
      >"${scratch}/${mode}.json"
}
render on
render off

# hubble_defaults <file> — both Deployments carry the pod default, every container
# carries an empty seLinuxOptions, and the chart's own hardening is still rendered.
hubble_defaults() {
  jq -e '[.[] | select(.kind == "Deployment" and
      (.metadata.name == "hubble-relay" or .metadata.name == "hubble-ui")) |
    .spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" and
    ([.spec.template.spec.containers[] | .securityContext.seLinuxOptions == {}] | all) and
    ([.spec.template.spec.containers[] | .securityContext.allowPrivilegeEscalation == false] | all) and
    ([.spec.template.spec.containers[] | .securityContext.capabilities.drop == ["ALL"]] | all)
  ] == [true, true]' "$1" >/dev/null
}
hubble_defaults "${scratch}/on.json" || fail 'hubble-relay or hubble-ui lacks the C-0211 defaults or lost chart hardening'
jq -e '[.[] | select(.kind == "Deployment" and .metadata.name == "hubble-relay") |
  .spec.template.spec.containers[0].securityContext |
  .runAsUser == 65532 and .runAsNonRoot == true and .readOnlyRootFilesystem == true] == [true]' \
  "${scratch}/on.json" >/dev/null || fail 'hubble-relay lost the chart default user or read-only root filesystem'

# Negative control: the release without the defaults must fail the positive check.
if hubble_defaults "${scratch}/off.json"; then
  fail 'negative control accepted a release without the Hubble defaults'
fi

# The chart must not already supply these fields; an inherited pod SELinux option
# would be shadowed by the empty container objects.
jq -e '[.[] | select(.kind == "Deployment" and
    (.metadata.name == "hubble-relay" or .metadata.name == "hubble-ui")) |
  .spec.template.spec.securityContext.fsGroupChangePolicy == null and
  .spec.template.spec.securityContext.seLinuxOptions == null and
  ([.spec.template.spec.containers[] | .securityContext.seLinuxOptions == null] | all)
] == [true, true]' "${scratch}/off.json" >/dev/null ||
  fail 'chart now supplies SELinux or fsGroupChangePolicy defaults; review before overriding them'

jq -S 'map(if .kind == "Deployment" and
    (.metadata.name == "hubble-relay" or .metadata.name == "hubble-ui") then
    del(.spec.template.spec.securityContext.fsGroupChangePolicy) |
    del(.spec.template.spec.containers[].securityContext.seLinuxOptions)
  else . end)' "${scratch}/on.json" >"${scratch}/normalized.json"
cmp -s "${scratch}/off.json" "${scratch}/normalized.json" ||
  fail 'the Hubble defaults change rendered resources beyond the two C-0211 fields'

printf 'PASS: Hubble relay and UI carry the two C-0211 defaults and change nothing else\n'
