#!/usr/bin/env bash

# Coroot helper Jobs use one immutable curl+jq image. A cached digest must not
# require a fresh Docker Hub token on every scheduled run.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly image_prefix='docker.io/badouralix/curl-jq:'
readonly digest_pattern='^docker[.]io/badouralix/curl-jq:[^[:space:]@]+@sha256:[0-9a-f]{64}$'
readonly production_overlay="${root_dir}/k8s/providers/hetzner/infrastructure"
readonly security_component="${root_dir}/k8s/bases/infrastructure/coroot/components/security-progress"
readonly security_consumer="${security_component}/cron-job.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

for tool in kubectl grep find yq sort cmp mktemp; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

grep -Fq -- "'scripts/tests/test-coroot-helper-image-pull-policy.sh'" "${ci_workflow}" ||
  fail 'a test-only edit must trigger the Kubernetes CI path'
grep -Fq -- 'bash scripts/tests/test-coroot-helper-image-pull-policy.sh' "${ci_workflow}" ||
  fail 'CI must execute the Coroot helper image policy test'

# Keep the disposable overlay beside k8s so all Kustomize references are relative.
scratch_dir="$(mktemp -d "${root_dir}/.coroot-helper-policy.XXXXXX")"
readonly scratch_dir
trap 'rm -rf "${scratch_dir}"' EXIT
: >"${scratch_dir}/all-sources"
: >"${scratch_dir}/production-sources"

# The reusable base stays dormant. Production may explicitly opt in through its
# component list; this test must also remain valid after that reviewed change.
security_enabled=false
while IFS= read -r component; do
  resolved="$(cd "${production_overlay}/${component}" && pwd)" ||
    fail "could not resolve production component: ${component}"
  if [ "${resolved}" = "${security_component}" ]; then security_enabled=true; fi
done < <(yq eval -r '(.components // [])[]' "${production_overlay}/kustomization.yaml")

manifests=()
while IFS= read -r manifest; do
  manifests+=("${manifest}")
done < <(find "${root_dir}/k8s" -type f -name '*.yaml' -exec grep -lF -- "${image_prefix}" {} +)
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
  identity="$(yq eval -r '(.metadata.namespace // "") + "/" + .metadata.name' "${manifest}")"
  printf '%s|%s|%s\n' "${identity}" "${image}" "${policy}" >>"${scratch_dir}/all-sources"
  if [ "${manifest}" != "${security_consumer}" ] || "${security_enabled}"; then
    printf '%s|%s|%s\n' "${identity}" "${image}" "${policy}" >>"${scratch_dir}/production-sources"
  fi
done
[ -f "${security_consumer}" ] || fail 'the optional security dashboard consumer must remain covered'

render_helpers() {
  local overlay=$1 output=$2
  kubectl kustomize "${overlay}" | yq eval -N -r '
    select(.kind == "CronJob") |
    select(.spec.jobTemplate.spec.template.spec.containers[].image | contains("docker.io/badouralix/curl-jq:")) |
    (.metadata.namespace // "") + "/" + .metadata.name + "|" +
    (.spec.jobTemplate.spec.template.spec.containers[] |
      select(.image | contains("docker.io/badouralix/curl-jq:")) | .image + "|" + (.imagePullPolicy // ""))
  ' - >"${output}" || fail "could not render Coroot helper Jobs from ${overlay}"
  local identity image policy
  while IFS='|' read -r identity image policy; do
    [[ "${image}" =~ ${digest_pattern} ]] ||
      fail "rendered Coroot helper image must retain an immutable digest: ${identity} in ${overlay}"
    [ "${policy}" = IfNotPresent ] ||
      fail "rendered Coroot helper Job must reuse its cached digest: ${identity} in ${overlay}"
  done <"${output}"
}

same_consumers() {
  cmp -s <(LC_ALL=C sort "$1") <(LC_ALL=C sort "$2")
}

render_helpers "${root_dir}/k8s/bases/infrastructure/coroot" "${scratch_dir}/base"
! grep -Fq -- 'observability/coroot-security-dashboards|' "${scratch_dir}/base" ||
  fail 'the reusable Coroot base must not activate security dashboards'

for layer in infrastructure controllers; do
  overlay="${production_overlay}"
  [ "${layer}" = infrastructure ] || overlay="${overlay}/controllers"
  render_helpers "${overlay}" "${scratch_dir}/${layer}"
  [ -s "${scratch_dir}/${layer}" ] || fail "no Coroot helper Jobs rendered from ${overlay}"
done
cat "${scratch_dir}/infrastructure" "${scratch_dir}/controllers" >"${scratch_dir}/production"
same_consumers "${scratch_dir}/production-sources" "${scratch_dir}/production" ||
  fail 'every explicitly activated source consumer must render exactly once in production'

mkdir "${scratch_dir}/enabled"
printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - %s\n' \
  '../../k8s/providers/hetzner/infrastructure' >"${scratch_dir}/enabled/kustomization.yaml"
if ! "${security_enabled}"; then
  printf 'components:\n  - %s\n' '../../k8s/bases/infrastructure/coroot/components/security-progress' >>"${scratch_dir}/enabled/kustomization.yaml"
fi
render_helpers "${scratch_dir}/enabled" "${scratch_dir}/enabled-infrastructure"
cat "${scratch_dir}/enabled-infrastructure" "${scratch_dir}/controllers" >"${scratch_dir}/enabled-all"
same_consumers "${scratch_dir}/all-sources" "${scratch_dir}/enabled-all" ||
  fail 'every authored source consumer must render exactly once when security dashboards are enabled'

# A removed consumer and a duplicated neighbor preserve the total count. The
# identity comparison must reject that case rather than letting them cancel out.
negative_records=()
while IFS= read -r record; do negative_records+=("${record}"); done <"${scratch_dir}/enabled-all"
negative_records[0]="${negative_records[1]}"
printf '%s\n' "${negative_records[@]}" >"${scratch_dir}/missing-and-duplicate"
if same_consumers "${scratch_dir}/all-sources" "${scratch_dir}/missing-and-duplicate"; then
  fail 'source/render comparison accepted a missing consumer masked by a duplicate'
fi

printf 'ok — %s Coroot curl+jq CronJobs use pinned, cached images across declared production and enabled-component renders\n' "${#manifests[@]}"
