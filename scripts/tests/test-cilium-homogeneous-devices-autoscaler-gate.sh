#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard_script="${root_dir}/scripts/guard-cilium-homogeneous-device-rollout.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"
readonly dr_runbook="${root_dir}/docs/dr/runbook.md"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -x "${guard_script}" ]] ||
  fail 'the rollout guard must be an executable script'

guard_calls="$(
  grep -nF './scripts/guard-cilium-homogeneous-device-rollout.sh' "${deploy_action}" |
    cut -d: -f1
)"
readonly guard_calls
guard_call_count="$(printf '%s\n' "${guard_calls}" | grep -c .)"
readonly guard_call_count
[[ "${guard_call_count}" -eq 2 ]] ||
  fail 'the deploy action must invoke the rollout guard exactly twice'
first_guard_call_line="$(printf '%s\n' "${guard_calls}" | sed -n '1p')"
second_guard_call_line="$(printf '%s\n' "${guard_calls}" | sed -n '2p')"
readonly first_guard_call_line second_guard_call_line

push_line="$(grep -nF 'run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push' "${deploy_action}" | cut -d: -f1)"
reconcile_line="$(grep -nF 'run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile' "${deploy_action}" | cut -d: -f1)"
cluster_update_line="$(grep -nF 'run: ./scripts/run-ksail-prod-with-pull-auth.sh cluster update' "${deploy_action}" | cut -d: -f1)"

((first_guard_call_line < push_line)) ||
  fail 'the first rollout guard must suspend autoscaling before publishing manifests'
((second_guard_call_line > reconcile_line && second_guard_call_line > cluster_update_line)) ||
  fail 'the second rollout guard must reassert or release the gate after deployment'

grep -Fq 'id: cilium_rollout_gate' "${deploy_action}" ||
  fail 'the pre-publish guard must expose whether the rollout gate is active'
grep -Fq "steps.cilium_rollout_gate.outputs.active != 'true'" "${deploy_action}" ||
  fail 'cluster update must remain skipped for the entire active rollout gate'

manual_dr_guard_calls="$(
  grep -nF './scripts/guard-cilium-homogeneous-device-rollout.sh' "${dr_runbook}" |
    cut -d: -f1 || true
)"
readonly manual_dr_guard_calls
manual_dr_guard_count="$(printf '%s\n' "${manual_dr_guard_calls}" | grep -c . || true)"
readonly manual_dr_guard_count
[[ "${manual_dr_guard_count}" -eq 2 ]] ||
  fail 'the manual DR fallback must invoke the rollout guard exactly twice'
manual_dr_guard_before_line="$(printf '%s\n' "${manual_dr_guard_calls}" | sed -n '1p')"
manual_dr_guard_after_line="$(printf '%s\n' "${manual_dr_guard_calls}" | sed -n '2p')"
manual_dr_push_line="$(grep -nF './scripts/run-ksail-prod-with-pull-auth.sh workload push' "${dr_runbook}" | cut -d: -f1)"
manual_dr_converged_line="$(grep -nF './scripts/refresh-flux-ghcr-auth.sh  # prove completed fan-out' "${dr_runbook}" | cut -d: -f1)"
readonly manual_dr_guard_before_line manual_dr_guard_after_line manual_dr_push_line manual_dr_converged_line

((manual_dr_guard_before_line < manual_dr_push_line)) ||
  fail 'the manual DR fallback must suspend autoscaling before publishing manifests'
((manual_dr_guard_after_line > manual_dr_converged_line)) ||
  fail 'the manual DR fallback must reassert or release the gate after Flux converges'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf -- "${tmp_dir}"' EXIT

fixture_root="${tmp_dir}/platform"
fixture_controllers="${fixture_root}/k8s/providers/hetzner/infrastructure/controllers"
fixture_component="${fixture_controllers}/cilium/components/homogeneous-devices"
mkdir -p "${fixture_component}"
cp "${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml" \
  "${fixture_controllers}/kustomization.yaml"
cp "${root_dir}/k8s/providers/hetzner/infrastructure/controllers/cilium/components/homogeneous-devices/kustomization.yaml" \
  "${fixture_component}/kustomization.yaml"

fake_kubectl="${tmp_dir}/kubectl"
fake_curl="${tmp_dir}/curl"
state_dir="${tmp_dir}/state"
mkdir -p "${state_dir}"
printf '1\n' >"${state_dir}/replicas"
printf '1\n' >"${state_dir}/status-replicas"
: >"${state_dir}/previous-replicas"
: >"${state_dir}/commands"
: >"${state_dir}/provider-commands"
printf '123\n' >"${state_dir}/provider-id"
: >"${state_dir}/github-output"

cat >"${fake_kubectl}" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${KUBECTL_STATE}/commands"

case "$*" in
  *"get nodes"*"-o json"*)
    printf '%s\n' '{"items":[{"metadata":{"name":"autoscale-test"},"spec":{"providerID":"hcloud://123"}}]}'
    ;;
  *"get deployment"*"cilium-device-rollout-previous-replicas"*)
    cat "${KUBECTL_STATE}/previous-replicas"
    ;;
  *"get deployment"*".spec.replicas"*)
    cat "${KUBECTL_STATE}/replicas"
    ;;
  *"get deployment"*".status.replicas"*)
    cat "${KUBECTL_STATE}/status-replicas"
    ;;
  *"annotate deployment"*"cilium-device-rollout-previous-replicas-"*)
    : >"${KUBECTL_STATE}/previous-replicas"
    ;;
  *"annotate deployment"*"cilium-device-rollout-previous-replicas="*)
    printf '%s\n' "$*" |
      sed 's/.*cilium-device-rollout-previous-replicas=\([0-9][0-9]*\).*/\1/' \
        >"${KUBECTL_STATE}/previous-replicas"
    ;;
  *"scale deployment"*"--replicas="*)
    printf '%s\n' "$*" | sed 's/.*--replicas=//' |
      tee "${KUBECTL_STATE}/replicas" >"${KUBECTL_STATE}/status-replicas"
    ;;
  *"rollout status deployment"*)
    ;;
  *)
    printf 'unexpected kubectl invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
FAKE_KUBECTL
chmod +x "${fake_kubectl}"

cat >"${fake_curl}" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${KUBECTL_STATE}/provider-commands"
provider_id="$(<"${KUBECTL_STATE}/provider-id")"
printf '{"servers":[{"id":%s}]}\n' "${provider_id}"
FAKE_CURL
chmod +x "${fake_curl}"

run_guard() {
  PLATFORM_ROOT="${fixture_root}" \
    KUBECTL="${fake_kubectl}" \
    CURL="${fake_curl}" \
    KUBECTL_STATE="${state_dir}" \
    GITHUB_OUTPUT="${state_dir}/github-output" \
    HCLOUD_TOKEN='test-token' \
    PROVIDER_STABILITY_SECONDS=0 \
    "${guard_script}" "$1"
}

run_guard --before-publish
[[ "$(tail -n 1 "${state_dir}/github-output")" == 'active=true' ]] ||
  fail 'the pre-publish phase must expose an active rollout gate to later steps'
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'an active OnDelete rollout must suspend the autoscaler before publish'
[[ "$(<"${state_dir}/previous-replicas")" == '1' ]] ||
  fail 'the rollout guard must remember the autoscaler replica count it owns'

printf '456\n' >"${state_dir}/provider-id"
if run_guard --before-publish; then
  fail 'a provider-side server without a matching Kubernetes node must fail closed'
fi
printf '123\n' >"${state_dir}/provider-id"

run_guard --after-deploy
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'an active OnDelete rollout must remain suspended after cluster update'
[[ "$(<"${state_dir}/previous-replicas")" == '1' ]] ||
  fail 'reasserting the gate must not overwrite the remembered replica count'

sed -i.bak '/type: OnDelete/d' "${fixture_component}/kustomization.yaml"
run_guard --before-publish
[[ "$(tail -n 1 "${state_dir}/github-output")" == 'active=false' ]] ||
  fail 'the pre-publish phase must release cluster update after the safe gate removal'
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'the pre-publish phase must not restore autoscaling before the safe artifact is deployed'
run_guard --after-deploy
[[ "$(<"${state_dir}/replicas")" == '1' ]] ||
  fail 'removing the rollout gate must restore the owned replica count after deployment'
[[ ! -s "${state_dir}/previous-replicas" ]] ||
  fail 'restoring autoscaling must release the rollout guard ownership marker'

printf '0\n' >"${state_dir}/replicas"
run_guard --after-deploy
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'an unowned manual autoscaler suspension must remain untouched'

if grep -Ev '(^|[[:space:]])--context admin@prod([[:space:]]|$)' "${state_dir}/commands" |
  grep -q .; then
  fail 'every autoscaler read and mutation must pin the admin@prod context'
fi
grep -Fq '.status.replicas' "${state_dir}/commands" ||
  fail 'the rollout guard must observe actual autoscaler replicas before publishing'
[[ -s "${state_dir}/provider-commands" ]] ||
  fail 'the rollout guard must fence Hetzner provider additions before publishing'
grep -Fq 'label_selector=hcloud/node-group' "${state_dir}/provider-commands" ||
  fail 'the provider fence must select the hcloud autoscaler node-group label'

printf 'PASS: Cilium activation suspends autoscaling before publish and restores only after the gate is removed\n'
