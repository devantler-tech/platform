#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard_script="${root_dir}/scripts/guard-cilium-homogeneous-device-rollout.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"
readonly dr_rebuild_workflow="${root_dir}/.github/workflows/dr-rebuild.yaml"
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

push_line="$(grep -nF 'id: publish_platform_manifest' "${deploy_action}" | cut -d: -f1)"
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
grep -Fq "CILIUM_ROLLOUT_REVISION_READY: \${{ steps.wait_flux_revision.outcome == 'success' }}" \
  "${deploy_action}" ||
  fail 'the post-deploy guard must approve a candidate only after the exact revision is Ready'
grep -Fq "CILIUM_ROLLOUT_REVISION_READY: \${{ steps.wait_flux_revision.outcome == 'success' }}" \
  "${dr_rebuild_workflow}" ||
  fail 'the DR rebuild guard must approve a candidate only after the exact revision is Ready'

manual_dr_guard_calls="$(
  grep -nF './scripts/guard-cilium-homogeneous-device-rollout.sh' "${dr_runbook}" |
    cut -d: -f1 || true
)"
readonly manual_dr_guard_calls
manual_dr_guard_count="$(printf '%s\n' "${manual_dr_guard_calls}" | grep -c . || true)"
readonly manual_dr_guard_count
[[ "${manual_dr_guard_count}" -eq 2 ]] ||
  fail 'the manual DR fallback must invoke the rollout guard exactly twice'
grep -Fq "> CILIUM_ROLLOUT_REVISION_READY=true \\" "${dr_runbook}" ||
  fail 'the manual DR fallback must mark approval only after its Flux readiness proof'
manual_dr_guard_before_line="$(printf '%s\n' "${manual_dr_guard_calls}" | sed -n '1p')"
manual_dr_guard_after_line="$(printf '%s\n' "${manual_dr_guard_calls}" | sed -n '2p')"
# The literal is copied from the runbook and must not expand in this test shell.
# shellcheck disable=SC2016
manual_dr_push_line="$(grep -nF 'PLATFORM_MANIFEST_DIGEST="$(docker buildx imagetools inspect' "${dr_runbook}" | cut -d: -f1)"
manual_dr_converged_line="$(grep -nF './scripts/refresh-flux-ghcr-auth.sh  # prove completed fan-out' "${dr_runbook}" | cut -d: -f1)"
readonly manual_dr_guard_before_line manual_dr_guard_after_line manual_dr_push_line manual_dr_converged_line

((manual_dr_guard_before_line < manual_dr_push_line)) ||
  fail 'the manual DR fallback must suspend autoscaling before selecting the active manifest'
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
printf '17\n' >"${state_dir}/cilium-generation"
printf '17\n' >"${state_dir}/cilium-observed-generation"
printf '16\n' >"${state_dir}/cilium-pod-generation"
printf '1\n' >"${state_dir}/cilium-desired"
printf '1\n' >"${state_dir}/cilium-current"
printf '1\n' >"${state_dir}/cilium-updated"
printf '1\n' >"${state_dir}/cilium-ready"
printf 'true\n' >"${state_dir}/cilium-pod-present"
printf 'approved\n' >"${state_dir}/cilium-template-value"
printf '2\n' >"${state_dir}/cilium-substitution-value"
: >"${state_dir}/approved-template-sha"
: >"${state_dir}/activated-at"
: >"${state_dir}/github-output"

cat >"${fake_kubectl}" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${KUBECTL_STATE}/commands"

case "$*" in
  kustomize*"clusters/prod/bootstrap"*)
    substitution_value="$(<"${KUBECTL_STATE}/cilium-substitution-value")"
    cat <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: variables-base
  namespace: flux-system
data:
  unrelated_base: unchanged
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: variables-cluster
  namespace: flux-system
data:
  cilium_replicas: "${substitution_value}"
---
apiVersion: v1
kind: Secret
metadata:
  name: variables-cluster
  namespace: flux-system
stringData:
  unrelated_secret: unchanged
EOF
    ;;
  kustomize*)
    template_value="$(<"${KUBECTL_STATE}/cilium-template-value")"
    cat <<EOF
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cilium
  namespace: kube-system
spec:
  chart:
    spec:
      version: 1.2.3
  values:
    devices: ${template_value}
    operator:
      replicas: \${cilium_replicas:=2}
    updateStrategy:
      type: OnDelete
  upgrade:
    disableWait: true
EOF
    ;;
  *"get daemonset cilium"*"-o json"*)
    generation="$(<"${KUBECTL_STATE}/cilium-generation")"
    observed_generation="$(<"${KUBECTL_STATE}/cilium-observed-generation")"
    desired="$(<"${KUBECTL_STATE}/cilium-desired")"
    current="$(<"${KUBECTL_STATE}/cilium-current")"
    updated="$(<"${KUBECTL_STATE}/cilium-updated")"
    ready="$(<"${KUBECTL_STATE}/cilium-ready")"
    printf '{"metadata":{"generation":%s},"status":{"observedGeneration":%s,"desiredNumberScheduled":%s,"currentNumberScheduled":%s,"updatedNumberScheduled":%s,"numberReady":%s}}\n' \
      "${generation}" "${observed_generation}" "${desired}" "${current}" "${updated}" "${ready}"
    ;;
  *"get pods"*"-l k8s-app=cilium"*"-o json"*)
    if [[ "$(<"${KUBECTL_STATE}/cilium-pod-present")" == 'true' ]]; then
      generation="$(<"${KUBECTL_STATE}/cilium-pod-generation")"
      printf '{"items":[{"metadata":{"labels":{"pod-template-generation":"%s"}},"spec":{"nodeName":"prod-worker-1"},"status":{"containerStatuses":[{"name":"cilium-agent","ready":true}]}}]}\n' \
        "${generation}"
    else
      printf '%s\n' '{"items":[]}'
    fi
    ;;
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
  *"get deployment"*"cilium-device-rollout-approved-template-sha"*)
    cat "${KUBECTL_STATE}/approved-template-sha"
    ;;
  *"annotate deployment"*"cilium-device-rollout-approved-template-sha-"*)
    : >"${KUBECTL_STATE}/approved-template-sha"
    ;;
  *"annotate deployment"*"cilium-device-rollout-approved-template-sha="*)
    printf '%s\n' "$*" |
      sed 's/.*cilium-device-rollout-approved-template-sha=\([0-9a-f][0-9a-f]*\).*/\1/' \
        >"${KUBECTL_STATE}/approved-template-sha"
    ;;
  *"get deployment"*"cilium-device-rollout-activated-at"*)
    cat "${KUBECTL_STATE}/activated-at"
    ;;
  *"annotate deployment"*"cilium-device-rollout-activated-at-"*)
    : >"${KUBECTL_STATE}/activated-at"
    ;;
  *"annotate deployment"*"cilium-device-rollout-activated-at="*)
    # Capture the value VERBATIM, not a digits-only sub-match: a digit capture
    # would truncate an ISO timestamp to its year and let the epoch-format
    # assertion pass for a formatted date — the exact regression it exists to
    # catch. Real kubectl stores the whole string, so the fake must too.
    printf '%s\n' "$*" |
      sed 's/.*cilium-device-rollout-activated-at=\([^ ]*\).*/\1/' \
        >"${KUBECTL_STATE}/activated-at"
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
  local phase="$1"
  local revision_ready="${2:-false}"

  PLATFORM_ROOT="${fixture_root}" \
    KUBECTL="${fake_kubectl}" \
    CURL="${fake_curl}" \
    KUBECTL_STATE="${state_dir}" \
    GITHUB_OUTPUT="${state_dir}/github-output" \
    HCLOUD_TOKEN='test-token' \
    CILIUM_ROLLOUT_REVISION_READY="${revision_ready}" \
    CILIUM_ROLLOUT_MAX_ACTIVE_SECONDS="${guard_max_active_seconds:-604800}" \
    PROVIDER_STABILITY_SECONDS=0 \
    "${guard_script}" "${phase}"
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

run_guard --after-deploy false
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'an active OnDelete rollout must remain suspended after cluster update'
[[ "$(<"${state_dir}/previous-replicas")" == '1' ]] ||
  fail 'reasserting the gate must not overwrite the remembered replica count'
[[ ! -s "${state_dir}/approved-template-sha" ]] ||
  fail 'a failed exact-revision wait must not approve the unpublished Cilium template'
run_guard --after-deploy true
[[ -s "${state_dir}/approved-template-sha" ]] ||
  fail 'the reconciled active gate must record the approved Cilium template hash'

sed -i.bak '/cilium\/components\/homogeneous-devices\//d' \
  "${fixture_controllers}/kustomization.yaml"
run_guard --before-publish
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'rolling back the component must keep autoscaling fenced before publish'
run_guard --after-deploy true
[[ "$(<"${state_dir}/replicas")" == '1' ]] ||
  fail 'a reconciled component rollback must restore the owned autoscaler replica count'
[[ ! -s "${state_dir}/approved-template-sha" ]] ||
  fail 'a reconciled component rollback must clear the approved template hash'
cp "${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml" \
  "${fixture_controllers}/kustomization.yaml"
run_guard --before-publish
run_guard --after-deploy true

sed -i.bak '/type: OnDelete/d' "${fixture_component}/kustomization.yaml"
printf 'changed\n' >"${state_dir}/cilium-template-value"
if run_guard --before-publish; then
  fail 'gate removal must reject an incoming non-strategy Cilium template change'
fi
printf 'approved\n' >"${state_dir}/cilium-template-value"
printf '17\n' >"${state_dir}/cilium-pod-generation"
printf '3\n' >"${state_dir}/cilium-substitution-value"
if run_guard --before-publish; then
  fail 'gate removal must reject a changed Flux substitution used by Cilium'
fi
printf '2\n' >"${state_dir}/cilium-substitution-value"
printf '0\n' >"${state_dir}/cilium-desired"
printf '0\n' >"${state_dir}/cilium-current"
printf '0\n' >"${state_dir}/cilium-updated"
printf '0\n' >"${state_dir}/cilium-ready"
printf 'false\n' >"${state_dir}/cilium-pod-present"
if run_guard --before-publish; then
  fail 'gate removal must reject an empty Cilium fleet'
fi
printf '1\n' >"${state_dir}/cilium-desired"
if run_guard --before-publish; then
  fail 'gate removal must reject a fleet missing a desired Cilium agent'
fi
printf '1\n' >"${state_dir}/cilium-current"
printf '1\n' >"${state_dir}/cilium-updated"
printf '1\n' >"${state_dir}/cilium-ready"
printf 'true\n' >"${state_dir}/cilium-pod-present"
printf '16\n' >"${state_dir}/cilium-observed-generation"
printf '17\n' >"${state_dir}/cilium-pod-generation"
if run_guard --before-publish; then
  fail 'gate removal must reject a DaemonSet controller that has not observed the current generation'
fi
printf '17\n' >"${state_dir}/cilium-observed-generation"
printf '16\n' >"${state_dir}/cilium-pod-generation"
printf '0\n' >"${state_dir}/cilium-updated"
if run_guard --before-publish; then
  fail 'gate removal must reject a DaemonSet that has not updated every desired agent'
fi
printf '1\n' >"${state_dir}/cilium-updated"
run_guard --before-publish
[[ "$(tail -n 1 "${state_dir}/github-output")" == 'active=false' ]] ||
  fail 'the pre-publish phase must release cluster update after the safe gate removal'
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'the pre-publish phase must not restore autoscaling before the safe artifact is deployed'
run_guard --after-deploy true
[[ "$(<"${state_dir}/replicas")" == '1' ]] ||
  fail 'removing the rollout gate must restore the owned replica count after deployment'
[[ ! -s "${state_dir}/previous-replicas" ]] ||
  fail 'restoring autoscaling must release the rollout guard ownership marker'

printf '0\n' >"${state_dir}/replicas"
run_guard --after-deploy true
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'an unowned manual autoscaler suspension must remain untouched'

# --- the gate's own window: an over-running gate must fail the deploy ---------
# Re-arm an active gate (the block above ended with the component rolled back
# and restored, so the ownership markers are clear).
cp "${root_dir}/k8s/providers/hetzner/infrastructure/controllers/cilium/components/homogeneous-devices/kustomization.yaml" \
  "${fixture_component}/kustomization.yaml"
printf '1\n' >"${state_dir}/replicas"
printf '1\n' >"${state_dir}/status-replicas"
: >"${state_dir}/previous-replicas"
: >"${state_dir}/activated-at"

# A gate that has never been timestamped — today's live prod state — must record
# its activation and stay green rather than reddening every deploy at once.
run_guard --before-publish
[[ -s "${state_dir}/activated-at" ]] ||
  fail 'an active gate with no recorded activation must start its window'
[[ "$(<"${state_dir}/activated-at")" =~ ^[0-9]+$ ]] ||
  fail 'the recorded gate activation must be epoch seconds, not a formatted date'
backfilled_at="$(<"${state_dir}/activated-at")"
readonly backfilled_at

# Inside the window: still green, and the recorded activation is not reset (a
# clock that restarted every deploy could never expire).
printf '%s\n' "$((backfilled_at - 3600))" >"${state_dir}/activated-at"
run_guard --before-publish
[[ "$(<"${state_dir}/activated-at")" == "$((backfilled_at - 3600))" ]] ||
  fail 'an active gate inside its window must not restart its own clock'

# Past the window: the deploy fails instead of silently skipping cluster update.
# Un-suspend the autoscaler first, so the suspension assertion below observes
# THIS run's suspension rather than one left behind by an earlier run — with
# replicas already 0 the assertion holds no matter what the guard does, and
# would pass even if the window check ran before the suspension.
printf '1\n' >"${state_dir}/replicas"
printf '1\n' >"${state_dir}/status-replicas"
printf '%s\n' "$((backfilled_at - 604801))" >"${state_dir}/activated-at"
if run_guard --before-publish; then
  fail 'a gate active past its window must fail the deploy'
fi
# ...and it stays fenced while it fails — the forcing function must not trade
# away the autoscaler suspension it is layered on top of.
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'a gate failing its window check must still suspend the autoscaler'

# The window is configurable for a deliberate, reviewed extension.
guard_max_active_seconds=$((604801 * 2))
run_guard --before-publish
unset guard_max_active_seconds

# A corrupt or future activation stamp fails closed rather than reading as fresh.
printf 'not-epoch\n' >"${state_dir}/activated-at"
if run_guard --before-publish; then
  fail 'a non-epoch activation stamp must fail closed'
fi
printf '%s\n' "$((backfilled_at + 86400))" >"${state_dir}/activated-at"
if run_guard --before-publish; then
  fail 'an activation stamp in the future must fail closed'
fi

# Releasing the gate clears the clock, so a later rollout starts a fresh window.
printf '%s\n' "${backfilled_at}" >"${state_dir}/activated-at"
sed -i.bak '/cilium\/components\/homogeneous-devices\//d' \
  "${fixture_controllers}/kustomization.yaml"
run_guard --after-deploy true
[[ ! -s "${state_dir}/activated-at" ]] ||
  fail 'releasing the rollout gate must clear its recorded activation time'

# A stamp must not outlive its rollout on the UNOWNED release path either. If a
# manual restore (or an interrupted release) clears the ownership marker while
# the stamp survives, the next rollout would inherit a stale clock, fail on
# arrival, and name only remedies that cannot fix a stale annotation.
printf '%s\n' "${backfilled_at}" >"${state_dir}/activated-at"
: >"${state_dir}/previous-replicas"
run_guard --after-deploy true
[[ ! -s "${state_dir}/activated-at" ]] ||
  fail 'an unowned gate release must still clear the recorded activation time'
cp "${root_dir}/k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml" \
  "${fixture_controllers}/kustomization.yaml"

if grep -Ev '(^kustomize[[:space:]]|(^|[[:space:]])--context admin@prod([[:space:]]|$))' "${state_dir}/commands" |
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
