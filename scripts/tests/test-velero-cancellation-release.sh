#!/usr/bin/env bash
# Exercise the pinned chart: a tag override must reach both backup processes.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
release="${root_dir}/k8s/bases/infrastructure/controllers/velero/helm-release.yaml"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/velero-cancellation.XXXXXX")"
trap 'rm -r -- "${scratch}"' EXIT
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

helm_bin="${CONTROLLER_HELM:-helm}"
chart_version="$(yq -er '.spec.chart.spec.version' "${release}")"
chart_repo="$(yq -er '.spec.url' "${root_dir}/k8s/bases/infrastructure/controllers/velero/helm-repository.yaml")"
kube_version="$(yq -er '.spec.cluster.kubernetesVersion' "${root_dir}/ksail.prod.yaml")"
"${helm_bin}" pull velero --repo "${chart_repo}" --version "${chart_version}" --destination "${scratch}" >/dev/null
kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/controllers" |
  yq ea 'select(.kind == "HelmRelease" and .metadata.name == "velero") | .spec.values' - >"${scratch}/values.yaml"
"${helm_bin}" template velero "${scratch}/velero-${chart_version}.tgz" --namespace velero \
  --kube-version "${kube_version}" --values "${scratch}/values.yaml" >"${scratch}/rendered.yaml"
yq ea -o=json -I=0 'select(.kind == "Deployment" or .kind == "DaemonSet")' \
  "${scratch}/rendered.yaml" >"${scratch}/workloads.json"

release_is_fixed() {
  jq -e -s '
    length == 2 and
    ([.[] | select(.kind == "Deployment" and .metadata.name == "velero")] | length) == 1 and
    ([.[] | select(.kind == "DaemonSet" and .metadata.name == "node-agent")] | length) == 1 and
    all(.[]; .metadata.namespace == "velero" and
      ([.spec.template.spec.containers[] | select(.name == "velero" or .name == "node-agent")] | length) == 1 and
      all(.spec.template.spec.containers[];
        .image | test("^velero/velero:v1\\.18\\.[0-9]+$") and
        (split(".")[-1] | tonumber) >= 4)) and
    all(.[] | select(.kind == "Deployment");
      .spec.replicas == 1 and .spec.strategy.type == "Recreate" and
      any(.spec.template.spec.initContainers[];
        .name == "velero-plugin-for-aws" and .image == "velero/velero-plugin-for-aws:v1.14.0"))
  ' "$1" >/dev/null
}

release_is_fixed "${scratch}/workloads.json" ||
  fail 'the actual server and node-agent must use a released 1.18 patch with the cancellation fix, preserving the AWS plugin and single-server rollout'

for mutation in \
  'if .kind == "Deployment" then .spec.template.spec.containers[0].image = "velero/velero:v1.18.2" else . end' \
  'if .kind == "DaemonSet" then .spec.template.spec.containers[0].image = "velero/velero:v1.18.2" else . end' \
  '.spec.template.spec.containers[0].image = "velero/velero:v1.19.0"' \
  'if .kind == "Deployment" then .spec.template.spec.initContainers[0].image = "velero/velero-plugin-for-aws:v1.14.2" else . end' \
  'if .kind == "Deployment" then .spec.replicas = 2 else . end'; do
  jq "${mutation}" "${scratch}/workloads.json" >"${scratch}/mutated.json"
  if release_is_fixed "${scratch}/mutated.json"; then
    fail "the release guard accepted mutation: ${mutation}"
  fi
done

grep -Fq "'scripts/tests/test-velero-cancellation-release.sh'" "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'the cancellation release regression must trigger CI'
grep -Fq 'bash scripts/tests/test-velero-cancellation-release.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must execute the cancellation release regression'
printf 'PASS: the pinned chart delivers the cancellation fix to both backup processes\n'
