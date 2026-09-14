#!/usr/bin/env bash

set -euo pipefail

readonly context='admin@prod'
readonly namespace='opencost'
readonly helmrelease='opencost'
readonly kubectl_bin="${KUBECTL_BIN:-kubectl}"
readonly git_bin="${GIT_BIN:-git}"
readonly repository_url='https://github.com/devantler-tech/platform.git'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

if [[ "$#" -ne 1 || ("$1" != '--preflight' && "$1" != '--execute') ]]; then
  fail 'OpenCost retirement requires the sole argument --execute or --preflight'
fi
[[ "${GITHUB_EVENT_NAME:-}" == 'workflow_dispatch' ]] ||
  fail 'OpenCost retirement is allowed only from a workflow_dispatch run'
[[ "${GITHUB_REF:-}" == 'refs/heads/main' ]] ||
  fail 'OpenCost retirement is allowed only from the full refs/heads/main ref'
[[ "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ]] ||
  fail 'OpenCost retirement requires a full lowercase GITHUB_SHA'
command -v "${git_bin}" >/dev/null 2>&1 || fail "git executable not found: ${git_bin}"

checkout_sha="$(${git_bin} rev-parse HEAD)"
remote_line="$(GIT_TERMINAL_PROMPT=0 "${git_bin}" ls-remote --exit-code \
  "${repository_url}" refs/heads/main)" ||
  fail 'unable to resolve the current main tip from the canonical repository'
read -r remote_sha remote_ref <<<"${remote_line}"
[[ "${remote_ref:-}" == 'refs/heads/main' && "${remote_sha:-}" =~ ^[0-9a-f]{40}$ ]] ||
  fail 'the canonical repository returned an invalid current main tip'
[[ "${checkout_sha}" == "${GITHUB_SHA}" && "${remote_sha}" == "${GITHUB_SHA}" ]] ||
  fail "refusing retirement because the workflow checkout is not the current main tip (event=${GITHUB_SHA}, checkout=${checkout_sha}, current=${remote_sha})"

if [[ "$1" == '--preflight' ]]; then
  printf 'PASS: OpenCost retirement is bound to the current main tip %s\n' "${GITHUB_SHA}"
  exit 0
fi

command -v "${kubectl_bin}" >/dev/null 2>&1 || fail "kubectl executable not found: ${kubectl_bin}"
command -v jq >/dev/null 2>&1 || fail 'jq is required to inspect Flux ownership'

kube() {
  "${kubectl_bin}" --context "${context}" "$@"
}

get_optional_json() {
  local resource="$1"
  shift
  kube get "${resource}" "$@" --ignore-not-found=true -o json
}

assert_prune_protected() {
  local description="$1"
  local object_json="$2"

  jq -e '
    .metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "disabled" and
    .metadata.labels["kustomize.toolkit.fluxcd.io/name"] == "infrastructure" and
    .metadata.labels["kustomize.toolkit.fluxcd.io/namespace"] == "flux-system" and
    (.metadata.annotations["platform.devantler.tech/prune-orphan"] // "") != "adopted" and
    ((.metadata.ownerReferences // []) | length) == 0
  ' <<<"${object_json}" >/dev/null ||
    fail "${description} is not the expected prune-protected Flux orphan"
}

assert_not_in_inventory() {
  local description="$1"
  local inventory_id="$2"
  local kustomization_json="$3"

  jq -e 'any(.status.conditions[]?; .type == "Ready" and .status == "True")' \
    <<<"${kustomization_json}" >/dev/null ||
    fail 'Flux Kustomization flux-system/infrastructure is not Ready'
  jq -e '(.status.inventory.entries | type) == "array"' \
    <<<"${kustomization_json}" >/dev/null ||
    fail 'Flux Kustomization flux-system/infrastructure does not expose a valid inventory entry list'
  if jq -e --arg id "${inventory_id}" \
    'any(.status.inventory.entries[]?; .id == $id)' \
    <<<"${kustomization_json}" >/dev/null; then
    fail "Flux still inventories ${description}"
  fi
}

helmrelease_json="$(get_optional_json \
  helmrelease.helm.toolkit.fluxcd.io/${helmrelease} --namespace "${namespace}")"
namespace_json="$(get_optional_json namespace/${namespace})"

if [[ -n "${helmrelease_json}" || -n "${namespace_json}" ]]; then
  kube wait --for=condition=Ready=True \
    kustomization.kustomize.toolkit.fluxcd.io/infrastructure \
    --namespace flux-system --timeout=2m ||
    fail 'Flux Kustomization flux-system/infrastructure did not become Ready within 2m'
  kustomization_json="$(kube get \
    kustomization.kustomize.toolkit.fluxcd.io/infrastructure \
    --namespace flux-system -o json)"

  if [[ -n "${helmrelease_json}" ]]; then
    assert_prune_protected 'HelmRelease opencost/opencost' "${helmrelease_json}"
    assert_not_in_inventory \
      'HelmRelease opencost/opencost' \
      'opencost_opencost_helm.toolkit.fluxcd.io_HelmRelease' \
      "${kustomization_json}"
  fi
  if [[ -n "${namespace_json}" ]]; then
    assert_prune_protected 'Namespace opencost' "${namespace_json}"
    assert_not_in_inventory \
      'Namespace opencost' \
      '_opencost__Namespace' \
      "${kustomization_json}"
  fi

  persistent_claims="$(kube get persistentvolumeclaims --namespace "${namespace}" -o name)"
  [[ -z "${persistent_claims}" ]] ||
    fail "refusing to retire OpenCost while PersistentVolumeClaims exist: ${persistent_claims//$'\n'/, }"
fi

if [[ -n "${helmrelease_json}" ]]; then
  kube delete helmrelease.helm.toolkit.fluxcd.io/${helmrelease} \
    --namespace "${namespace}" --wait=false
  kube wait --for=delete helmrelease.helm.toolkit.fluxcd.io/${helmrelease} \
    --namespace "${namespace}" --timeout=5m
fi

cluster_role="$(kube get clusterrole.rbac.authorization.k8s.io/opencost \
  --ignore-not-found=true -o name)"
cluster_role_binding="$(kube get clusterrolebinding.rbac.authorization.k8s.io/opencost \
  --ignore-not-found=true -o name)"
[[ -z "${cluster_role}" && -z "${cluster_role_binding}" ]] ||
  fail "the OpenCost chart uninstall left cluster RBAC behind: ${cluster_role} ${cluster_role_binding}"

if [[ -n "${namespace_json}" ]]; then
  kube delete namespace/${namespace} --wait=false
  kube wait --for=delete namespace/${namespace} --timeout=5m
fi

[[ -z "$(get_optional_json helmrelease.helm.toolkit.fluxcd.io/${helmrelease} --namespace "${namespace}")" ]] ||
  fail 'HelmRelease opencost/opencost still exists after retirement'
[[ -z "$(get_optional_json namespace/${namespace})" ]] ||
  fail 'Namespace opencost still exists after retirement'

job_suffix="${OPENCOST_RETIRE_JOB_SUFFIX:-${GITHUB_RUN_ID:-manual}-${GITHUB_RUN_ATTEMPT:-1}}"
[[ "${job_suffix}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
  fail "invalid orphan-check Job suffix: ${job_suffix}"
readonly verification_job="prune-protected-orphan-check-opencost-${job_suffix}"
[[ "${#verification_job}" -le 63 ]] || fail 'orphan-check Job name exceeds 63 characters'

verification_job_created='false'
cleanup_verification_job() {
  if [[ "${verification_job_created}" == 'true' ]]; then
    kube delete "job/${verification_job}" --namespace observability \
      --ignore-not-found=true --wait=false >/dev/null
  fi
}
trap cleanup_verification_job EXIT

kube create job --from=cronjob/prune-protected-orphan-alert \
  "${verification_job}" --namespace observability
verification_job_created='true'
if ! kube wait --for=condition=complete "job/${verification_job}" \
  --namespace observability --timeout=4m; then
  kube logs "job/${verification_job}" --namespace observability || true
  fail 'the independent prune-protected-orphan check did not succeed'
fi
verification_output="$(kube logs "job/${verification_job}" --namespace observability)"
printf '%s\n' "${verification_output}"
if grep -Eq 'HelmRelease opencost/opencost|Namespace opencost' <<<"${verification_output}"; then
  fail 'the independent orphan check still reports OpenCost'
fi

printf 'PASS: OpenCost production retirement completed\n'
