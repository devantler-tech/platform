#!/usr/bin/env bash

# Render the real chart: only the rendered hook shows what the post-renderer changes (#5491).
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
release="${KYVERNO_RELEASE_FILE:-${root_dir}/k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml}"
repository="${root_dir}/k8s/bases/infrastructure/controllers/kyverno/helm-repository.yaml"
scratch="$(mktemp -d)"
readonly root_dir release repository scratch
trap 'rm -rf "${scratch}"' EXIT

fail() { printf 'FAIL: kyverno migration hook: %s\n' "$1" >&2; exit 1; }
for tool in helm jq kubectl yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

job='kyverno-migrate-resources'
image='ghcr.io/devantler-tech/platform-kyverno-migrate:v1.19.2-migration.1-eed5caab082e10a1ee7c02ac636b66f257c4cfb3@sha256:ee142d6feae09ff8970286261a17ce6f6e9eb0c848a601db31c78933a189ef42'

[[ "$(yq -r '.spec.postRenderStrategy' "${release}")" == combined ]] ||
  fail 'hooks reach a post-renderer only through the combined strategy'

chart="$(yq -r '.spec.chart.spec.chart' "${release}")"
version="$(yq -r '.spec.chart.spec.version' "${release}")"
url="$(yq -r '.spec.url' "${repository}")"
yq -o=json '.spec.values' "${release}" >"${scratch}/values.json"
helm template kyverno "${chart}" --repo "${url}" --version "${version}" --is-upgrade \
  --namespace kyverno --values "${scratch}/values.json" >"${scratch}/resources.yaml"
# Kustomize normalizes what it reads, so the comparison baseline goes through it unpatched.
printf 'resources: [resources.yaml]\n' >"${scratch}/kustomization.yaml"
kubectl kustomize "${scratch}" >"${scratch}/before.yaml"

renderer_count="$(yq '.spec.postRenderers | length' "${release}")"
for ((index = 0; index < renderer_count; index++)); do
  yq -o=json ".spec.postRenderers[${index}].kustomize" "${release}" >"${scratch}/renderer.json"
  jq -n --slurpfile renderer "${scratch}/renderer.json" \
    '{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
      resources: ["resources.yaml"]} + $renderer[0]' >"${scratch}/kustomization.yaml"
  kubectl kustomize "${scratch}" >"${scratch}/next.yaml"
  mv "${scratch}/next.yaml" "${scratch}/resources.yaml"
done

normalized() {
  yq ea -o=json '[. | select(. != null)]' "$1" |
    jq -S 'map({key: "\(.kind)/\(.metadata.namespace // "")/\(.metadata.name)", value: .}) | from_entries'
}
normalized "${scratch}/before.yaml" >"${scratch}/before.json"
normalized "${scratch}/resources.yaml" >"${scratch}/after.json"
key="Job/kyverno/${job}"

# Failing-before: the chart itself must still need this patch, or the patch should go.
jq -e --arg key "${key}" '.[$key].metadata.annotations | has("helm.sh/hook-delete-policy") | not' \
  "${scratch}/before.json" >/dev/null || fail 'the chart now sets a deletion policy; retire this patch'

jq -e --arg key "${key}" '.[$key].metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded"' \
  "${scratch}/after.json" >/dev/null || fail 'a successful Job must be deleted and a failed one kept'
jq -e --arg key "${key}" '.[$key].metadata.annotations["helm.sh/hook"] == "post-upgrade"' \
  "${scratch}/after.json" >/dev/null || fail 'the Job must remain the post-upgrade hook'
jq -e --arg key "${key}" --arg image "${image}" '[.[$key].spec.template.spec.containers[].image] == [$image]' \
  "${scratch}/after.json" >/dev/null || fail 'the Job must run the verified migration runtime by digest'

# Nothing else may move: the patch is scoped to one annotation on one object.
jq -e --arg key "${key}" --slurpfile before "${scratch}/before.json" '
  del(.[$key].metadata.annotations["helm.sh/hook-delete-policy"]) == $before[0]
' "${scratch}/after.json" >/dev/null || fail 'the post-renderer changed something other than the Job deletion policy'
jq -e '[to_entries[] | select(.value.kind == "Job" and .value.metadata.annotations["helm.sh/hook"] != null)] | length > 1' \
  "${scratch}/after.json" >/dev/null || fail 'expected unrelated hook Jobs to prove they stay unchanged'

grep -Fq 'scripts/tests/test-kyverno-migration-hook-lifecycle.sh' "${root_dir}/.github/workflows/ci.yaml" ||
  fail 'CI must run this test'
echo 'kyverno migration hook: PASS'
