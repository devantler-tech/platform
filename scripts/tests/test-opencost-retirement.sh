#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly subject="${root_dir}/scripts/retire-opencost.sh"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

temp_dir="$(mktemp -d)"
readonly temp_dir
trap 'rm -rf "${temp_dir}"' EXIT

readonly fake_kubectl="${temp_dir}/kubectl"
cat >"${fake_kubectl}" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail

: "${FAKE_STATE_DIR:?}"
printf '%s\n' "$*" >>"${FAKE_STATE_DIR}/commands.log"

if [[ "${1:-}" != '--context' || "${2:-}" != 'admin@prod' ]]; then
  printf 'unexpected context: %s\n' "$*" >&2
  exit 64
fi
shift 2

metadata() {
  jq -n '{
    metadata: {
      annotations: {"kustomize.toolkit.fluxcd.io/prune": "disabled"},
      labels: {
        "kustomize.toolkit.fluxcd.io/name": "infrastructure",
        "kustomize.toolkit.fluxcd.io/namespace": "flux-system"
      }
    }
  }'
}

case "$*" in
  'get helmrelease.helm.toolkit.fluxcd.io/opencost --namespace opencost --ignore-not-found=true -o json')
    [[ ! -e "${FAKE_STATE_DIR}/helmrelease" ]] || metadata
    ;;
  'get namespace/opencost --ignore-not-found=true -o json')
    [[ ! -e "${FAKE_STATE_DIR}/namespace" ]] || metadata
    ;;
  'get persistentvolumeclaims --namespace opencost -o name')
    [[ ! -e "${FAKE_STATE_DIR}/pvc" ]] || printf '%s\n' 'persistentvolumeclaim/opencost-data'
    ;;
  'get kustomization.kustomize.toolkit.fluxcd.io/infrastructure --namespace flux-system --request-timeout=1s -o json')
    if [[ -e "${FAKE_STATE_DIR}/ready-timeout" ]]; then
      jq -n '{status:{conditions:[{type:"Ready",status:"False"}],inventory:{entries:[{id:"observability_coroot_operator_helm.toolkit.fluxcd.io_HelmRelease"}]}}}'
    elif [[ -e "${FAKE_STATE_DIR}/transient-not-ready" ]]; then
      rm -f "${FAKE_STATE_DIR}/transient-not-ready"
      jq -n '{status:{conditions:[{type:"Ready",status:"False"}],inventory:{entries:[{id:"observability_coroot_operator_helm.toolkit.fluxcd.io_HelmRelease"}]}}}'
    elif [[ -e "${FAKE_STATE_DIR}/missing-inventory" ]]; then
      jq -n '{status:{conditions:[{type:"Ready",status:"True"}]}}'
    elif [[ -e "${FAKE_STATE_DIR}/managed-helmrelease" ]]; then
      jq -n '{status:{conditions:[{type:"Ready",status:"True"}],inventory:{entries:[{id:"opencost_opencost_helm.toolkit.fluxcd.io_HelmRelease"}]}}}'
    else
      jq -n '{status:{conditions:[{type:"Ready",status:"True"}],inventory:{entries:[{id:"observability_coroot_operator_helm.toolkit.fluxcd.io_HelmRelease"}]}}}'
    fi
    ;;
  'delete helmrelease.helm.toolkit.fluxcd.io/opencost --namespace opencost --wait=false')
    rm -f "${FAKE_STATE_DIR}/helmrelease" "${FAKE_STATE_DIR}/clusterrole" "${FAKE_STATE_DIR}/clusterrolebinding"
    ;;
  'wait --for=delete helmrelease.helm.toolkit.fluxcd.io/opencost --namespace opencost --timeout=5m')
    [[ ! -e "${FAKE_STATE_DIR}/helmrelease" ]]
    ;;
  'get clusterrole.rbac.authorization.k8s.io/opencost --ignore-not-found=true -o name')
    [[ ! -e "${FAKE_STATE_DIR}/clusterrole" ]] || printf '%s\n' 'clusterrole.rbac.authorization.k8s.io/opencost'
    ;;
  'get clusterrolebinding.rbac.authorization.k8s.io/opencost --ignore-not-found=true -o name')
    [[ ! -e "${FAKE_STATE_DIR}/clusterrolebinding" ]] || printf '%s\n' 'clusterrolebinding.rbac.authorization.k8s.io/opencost'
    ;;
  'delete namespace/opencost --wait=false')
    [[ ! -e "${FAKE_STATE_DIR}/helmrelease" ]] || exit 65
    rm -f "${FAKE_STATE_DIR}/namespace"
    ;;
  'wait --for=delete namespace/opencost --timeout=5m')
    [[ ! -e "${FAKE_STATE_DIR}/namespace" ]]
    ;;
  'create job --from=cronjob/prune-protected-orphan-alert prune-protected-orphan-check-opencost-test --namespace observability')
    touch "${FAKE_STATE_DIR}/job"
    ;;
  'wait --for=condition=complete job/prune-protected-orphan-check-opencost-test --namespace observability --timeout=4m')
    [[ -e "${FAKE_STATE_DIR}/job" ]]
    ;;
  'logs job/prune-protected-orphan-check-opencost-test --namespace observability')
    if [[ -e "${FAKE_STATE_DIR}/orphan-finding" ]]; then
      printf '%s\n' 'WARNING: HelmRelease opencost/opencost is left behind'
    else
      printf '%s\n' 'Checked 8 prune-protected Flux resource(s); none left behind for 604800s or longer.'
    fi
    ;;
  'delete job/prune-protected-orphan-check-opencost-test --namespace observability --ignore-not-found=true --wait=false')
    rm -f "${FAKE_STATE_DIR}/job"
    ;;
  *)
    printf 'unexpected kubectl invocation: %s\n' "$*" >&2
    exit 66
    ;;
esac
FAKE
chmod +x "${fake_kubectl}"

readonly expected_head='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
readonly fake_git="${temp_dir}/git"
cat >"${fake_git}" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail

: "${FAKE_STATE_DIR:?}"
case "$*" in
  'rev-parse HEAD')
    printf '%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    ;;
  'ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main')
    if [[ -e "${FAKE_STATE_DIR}/stale-main" ]]; then
      printf '%s\t%s\n' 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' 'refs/heads/main'
    else
      printf '%s\t%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' 'refs/heads/main'
    fi
    ;;
  *)
    printf 'unexpected git invocation: %s\n' "$*" >&2
    exit 67
    ;;
esac
FAKE
chmod +x "${fake_git}"

init_state() {
  local name="$1"
  local state="${temp_dir}/${name}"
  mkdir -p "${state}"
  touch "${state}/helmrelease" "${state}/namespace" "${state}/clusterrole" "${state}/clusterrolebinding"
  : >"${state}/commands.log"
  printf '%s\n' "${state}"
}

run_subject() {
  local state="$1"
  shift
  FAKE_STATE_DIR="${state}" \
    KUBECTL_BIN="${fake_kubectl}" \
    GIT_BIN="${fake_git}" \
    OPENCOST_RETIRE_JOB_SUFFIX=test \
    OPENCOST_RETIRE_POLL_SECONDS=0 \
    OPENCOST_RETIRE_REQUEST_TIMEOUT_SECONDS=1 \
    OPENCOST_RETIRE_TIMEOUT_SECONDS=3 \
    GITHUB_EVENT_NAME=workflow_dispatch \
    GITHUB_REF=refs/heads/main \
    GITHUB_REF_NAME=main \
    GITHUB_SHA="${expected_head}" \
    bash "${subject}" "$@"
}

preflight_state="$(init_state preflight)"
if ! preflight_output="$(run_subject "${preflight_state}" --preflight 2>&1)"; then
  fail "the matching current-main preflight should succeed: ${preflight_output}"
fi
grep -qF 'PASS: OpenCost retirement is bound to the current main tip' <<<"${preflight_output}" ||
  fail 'the current-main preflight did not report its exact-tip proof'
[[ ! -s "${preflight_state}/commands.log" ]] ||
  fail 'the current-main preflight contacted Kubernetes'

stale_state="$(init_state stale)"
touch "${stale_state}/stale-main"
if run_subject "${stale_state}" --execute >"${temp_dir}/stale.out" 2>&1; then
  fail 'retirement must refuse a workflow whose recorded SHA is no longer the current main tip'
fi
grep -qF 'current main tip' "${temp_dir}/stale.out" ||
  fail 'the stale-main refusal did not explain the tip mismatch'
[[ ! -s "${stale_state}/commands.log" ]] ||
  fail 'the stale-main refusal contacted Kubernetes'

happy_state="$(init_state happy)"
if ! happy_output="$(run_subject "${happy_state}" --execute 2>&1)"; then
  fail "the safe orphan retirement should succeed: ${happy_output}"
fi
for removed in helmrelease namespace clusterrole clusterrolebinding job; do
  [[ ! -e "${happy_state}/${removed}" ]] || fail "successful retirement left ${removed} behind"
done
helm_delete_line="$(grep -nFx -- '--context admin@prod delete helmrelease.helm.toolkit.fluxcd.io/opencost --namespace opencost --wait=false' "${happy_state}/commands.log" | cut -d: -f1)"
namespace_delete_line="$(grep -nFx -- '--context admin@prod delete namespace/opencost --wait=false' "${happy_state}/commands.log" | cut -d: -f1)"
[[ "${helm_delete_line}" -lt "${namespace_delete_line}" ]] || fail 'the HelmRelease must be deleted before its Namespace'
grep -qF 'PASS: OpenCost production retirement completed' <<<"${happy_output}" ||
  fail 'successful retirement did not report its completion'

transient_ready_state="$(init_state transient-ready)"
touch "${transient_ready_state}/transient-not-ready"
if ! transient_ready_output="$(run_subject "${transient_ready_state}" --execute 2>&1)"; then
  fail "retirement should wait for a transient Flux reconciliation: ${transient_ready_output}"
fi
transient_get_count="$(grep -cFx -- '--context admin@prod get kustomization.kustomize.toolkit.fluxcd.io/infrastructure --namespace flux-system --request-timeout=1s -o json' \
  "${transient_ready_state}/commands.log")"
[[ "${transient_get_count}" -ge 2 ]] ||
  fail 'retirement did not poll for a single Ready inventory snapshot'

ready_timeout_state="$(init_state ready-timeout)"
touch "${ready_timeout_state}/ready-timeout"
ready_timeout_started="${SECONDS}"
if run_subject "${ready_timeout_state}" --execute >"${temp_dir}/ready-timeout.out" 2>&1; then
  fail 'retirement must refuse deletion when Flux does not become Ready in time'
fi
ready_timeout_elapsed="$((SECONDS - ready_timeout_started))"
[[ "${ready_timeout_elapsed}" -le 4 ]] ||
  fail "the Ready polling deadline took ${ready_timeout_elapsed}s despite a 3s limit"
grep -qF 'did not expose a Ready inventory snapshot within 3s' "${temp_dir}/ready-timeout.out" ||
  fail 'the Ready timeout did not explain the unjudgeable Flux state'
grep -qF 'delete helmrelease' "${ready_timeout_state}/commands.log" &&
  fail 'the Ready timeout issued a destructive command'

unarmed_state="$(init_state unarmed)"
if run_subject "${unarmed_state}" >"${temp_dir}/unarmed.out" 2>&1; then
  fail 'retirement must be default-off without the explicit execution flag'
fi
grep -qF 'requires the sole argument --execute' "${temp_dir}/unarmed.out" ||
  fail 'the default-off refusal did not explain the required execution flag'
grep -qF 'delete helmrelease' "${unarmed_state}/commands.log" &&
  fail 'the default-off refusal issued a destructive command'

pvc_state="$(init_state pvc)"
touch "${pvc_state}/pvc"
if run_subject "${pvc_state}" --execute >"${temp_dir}/pvc.out" 2>&1; then
  fail 'retirement must refuse a namespace that contains a PersistentVolumeClaim'
fi
grep -qF 'refusing to retire OpenCost while PersistentVolumeClaims exist' "${temp_dir}/pvc.out" ||
  fail 'the PVC refusal did not explain the protected dependency'
grep -qF 'delete helmrelease' "${pvc_state}/commands.log" &&
  fail 'the PVC refusal issued a destructive command'

managed_state="$(init_state managed)"
touch "${managed_state}/managed-helmrelease"
if run_subject "${managed_state}" --execute >"${temp_dir}/managed.out" 2>&1; then
  fail 'retirement must refuse a HelmRelease that Flux still inventories'
fi
grep -qF 'still inventories HelmRelease opencost/opencost' "${temp_dir}/managed.out" ||
  fail 'the managed-resource refusal did not explain the ownership conflict'
grep -qF 'delete helmrelease' "${managed_state}/commands.log" &&
  fail 'the managed-resource refusal issued a destructive command'

missing_inventory_state="$(init_state missing-inventory)"
touch "${missing_inventory_state}/missing-inventory"
if run_subject "${missing_inventory_state}" --execute >"${temp_dir}/missing-inventory.out" 2>&1; then
  fail 'retirement must refuse a Kustomization without a valid inventory entry list'
fi
grep -qF 'does not expose a valid inventory entry list' "${temp_dir}/missing-inventory.out" ||
  fail 'the missing-inventory refusal did not explain the unjudgeable ownership state'
grep -qF 'delete helmrelease' "${missing_inventory_state}/commands.log" &&
  fail 'the missing-inventory refusal issued a destructive command'

finding_state="$(init_state finding)"
touch "${finding_state}/orphan-finding"
if run_subject "${finding_state}" --execute >"${temp_dir}/finding.out" 2>&1; then
  fail 'retirement must fail when the independent orphan check still names OpenCost'
fi
grep -qF 'the independent orphan check still reports OpenCost' "${temp_dir}/finding.out" ||
  fail 'the orphan-check failure did not explain the remaining finding'

printf 'PASS: OpenCost retirement is ordered, default-safe, PVC-safe, ownership-safe, and independently verified\n'
