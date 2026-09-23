#!/usr/bin/env bash

# Coroot helper Jobs use one immutable curl+jq image. A cached digest must not
# require a fresh Docker Hub token on every scheduled run.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly image_prefix='docker.io/badouralix/curl-jq:'
readonly digest_pattern='^docker[.]io/badouralix/curl-jq:[^[:space:]@]+@sha256:[0-9a-f]{64}$'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

for tool in kubectl rg yq; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

rg -Fq "'scripts/tests/test-coroot-helper-image-pull-policy.sh'" "${ci_workflow}" ||
  fail 'a test-only edit must trigger the Kubernetes CI path'
rg -Fq 'bash scripts/tests/test-coroot-helper-image-pull-policy.sh' "${ci_workflow}" ||
  fail 'CI must execute the Coroot helper image policy test'

manifests=()
while IFS= read -r manifest; do
  manifests+=("${manifest}")
done < <(rg -l -F --glob '*.yaml' "${image_prefix}" "${root_dir}/k8s")
[ "${#manifests[@]}" -ge 11 ] ||
  fail 'expected every existing Coroot curl+jq CronJob consumer to remain covered'

for manifest in "${manifests[@]}"; do
  [ "$(yq eval -r '.kind' "${manifest}")" = CronJob ] ||
    fail "curl+jq consumer is not a CronJob: ${manifest}"
  image="$(yq eval -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.image | contains("docker.io/badouralix/curl-jq:")) | .image' "${manifest}")"
  policy="$(yq eval -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.image | contains("docker.io/badouralix/curl-jq:")) | .imagePullPolicy' "${manifest}")"
  [[ "${image}" =~ ${digest_pattern} ]] ||
    fail "curl+jq image must retain an immutable digest: ${manifest}"
  [ "${policy}" = IfNotPresent ] ||
    fail "cached curl+jq digest must not reauthenticate on every run: ${manifest}"
done

rendered_count=0
for overlay in \
  "${root_dir}/k8s/providers/hetzner/infrastructure" \
  "${root_dir}/k8s/providers/hetzner/infrastructure/controllers"; do
  rendered_images="$(kubectl kustomize "${overlay}" | yq ea -N -r '
    select(.kind == "CronJob") |
    .spec.jobTemplate.spec.template.spec.containers[] |
    select(.image | contains("docker.io/badouralix/curl-jq:")) |
    .image + "|" + (.imagePullPolicy // "")
  ' -)" || fail "could not render Coroot helper Jobs from ${overlay}"
  [ -n "${rendered_images}" ] || fail "no Coroot helper Jobs rendered from ${overlay}"
  while IFS='|' read -r image policy; do
    [[ "${image}" =~ ${digest_pattern} ]] ||
      fail "rendered Coroot helper image must retain an immutable digest: ${overlay}"
    [ "${policy}" = IfNotPresent ] ||
      fail "rendered Coroot helper Job must reuse its cached digest: ${overlay}"
    rendered_count=$((rendered_count + 1))
  done <<< "${rendered_images}"
done
[ "${rendered_count}" -eq "${#manifests[@]}" ] ||
  fail 'every source consumer must render exactly once in the production infrastructure'

printf 'ok — %s Coroot curl+jq CronJobs use pinned, cached images\n' "${#manifests[@]}"
