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
[[ "${guard_call_count}" -eq 3 ]] ||
  fail 'the deploy action must invoke the rollout guard exactly three times'
first_guard_call_line="$(printf '%s\n' "${guard_calls}" | sed -n '1p')"
second_guard_call_line="$(printf '%s\n' "${guard_calls}" | sed -n '2p')"
third_guard_call_line="$(printf '%s\n' "${guard_calls}" | sed -n '3p')"
readonly first_guard_call_line second_guard_call_line third_guard_call_line

push_line="$(grep -nF 'id: publish_platform_manifest' "${deploy_action}" | cut -d: -f1)"
# The deploy composite reconciles through the wrapper that tolerates a
# control-plane restart it caused itself (#3478).
reconcile_line="$(grep -nF 'run: ./scripts/reconcile-flux-workloads.sh' "${deploy_action}" | cut -d: -f1)"
revision_line="$(grep -nF 'id: wait_flux_revision' "${deploy_action}" | cut -d: -f1)"
cluster_update_line="$(grep -nF 'run: ./scripts/run-ksail-prod-with-pull-auth.sh cluster update' "${deploy_action}" | cut -d: -f1)"

((first_guard_call_line < push_line)) ||
  fail 'the first rollout guard must suspend autoscaling before publishing manifests'
# The middle call restores a RELEASED gate's suspension between the
# exact-revision proof and cluster update. Both bounds are load-bearing: after
# the proof because autoscaling may not resume before the safe artifact is
# deployed, and before cluster update because that step waits for the
# cluster-autoscaler Deployment and KSail treats zero replicas as never-ready —
# a released gate would otherwise hang the deploy that releases it.
((second_guard_call_line > revision_line && second_guard_call_line < cluster_update_line)) ||
  fail 'the second rollout guard must restore autoscaling after the revision proof and before cluster update'
((third_guard_call_line > reconcile_line && third_guard_call_line > cluster_update_line)) ||
  fail 'the third rollout guard must reassert or release the gate after deployment'

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

# CONSTRUCT the active-gate state these assertions exercise instead of
# inheriting whatever the repository currently ships (platform#3031). The
# overrides below exist only while a rollout is being stepped, so once one
# completes and they are removed, an inherited fixture starts INACTIVE and every
# active-gate assertion here either fails for the wrong reason or passes
# vacuously — the test would stop covering the gate exactly when the gate is
# most likely to be reintroduced incorrectly. Appending keeps the block valid:
# these lines extend the component's trailing `patch: |` literal.
if ! grep -Eq '^[[:space:]]*type:[[:space:]]*OnDelete[[:space:]]*$' \
  "${fixture_component}/kustomization.yaml"; then
  cat >>"${fixture_component}/kustomization.yaml" <<'ACTIVE_GATE'
      - op: replace
        path: /spec/values/updateStrategy
        value:
          rollingUpdate: null
          type: OnDelete
      - op: add
        path: /spec/upgrade/disableWait
        value: true
ACTIVE_GATE
fi

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
# `ksail cluster update` waits for the cluster-autoscaler Deployment and KSail
# treats zero replicas as never-ready, so a released gate MUST hand that step a
# running autoscaler. Restoring only after deployment fails the very deploy that
# releases the gate — observed in prod on 2026-08-09, where cluster update timed
# out polling a Deployment the gate itself held at zero.
run_guard --after-revision-ready
[[ "$(<"${state_dir}/replicas")" == '1' ]] ||
  fail 'a released gate must restore autoscaling before cluster update waits on it'
[[ ! -s "${state_dir}/previous-replicas" ]] ||
  fail 'restoring autoscaling must release the rollout guard ownership marker'
run_guard --after-deploy true
[[ "$(<"${state_dir}/replicas")" == '1' ]] ||
  fail 'the post-deploy phase must leave an already-restored autoscaler running'

# A remembered count of ZERO — the autoscaler was already scaled down when the
# gate claimed it — cannot be "restored" into something cluster update can wait
# on. Honouring it would hand that step the same never-ready Deployment AND
# clear the ownership marker a retry needs, so the release must fail loudly and
# keep the marker instead.
printf '0\n' >"${state_dir}/previous-replicas"
printf '0\n' >"${state_dir}/replicas"
# Pass revision_ready=true and assert the MESSAGE, not merely a non-zero exit:
# run_guard defaults that flag to false, so a phase that rejected it would fail
# for an unrelated reason and this test would pass without ever reaching the
# zero-count branch it exists to cover.
if zero_count_output="$(run_guard --after-revision-ready true 2>&1)"; then
  fail 'releasing a gate that owns a remembered zero replica count must fail loudly'
fi
[[ "${zero_count_output}" == *'remembered autoscaler count of 0'* ]] ||
  fail "the zero-count refusal must name the conflict; got: ${zero_count_output}"
# The remediation has to change the REMEMBERED value, or an operator who
# follows it hits this same refusal on the retry.
[[ "${zero_count_output}" == *"${previous_replicas_annotation:-cilium-device-rollout-previous-replicas}"* ]] ||
  fail "the zero-count refusal must name the annotation that records the count; got: ${zero_count_output}"
[[ "$(<"${state_dir}/previous-replicas")" == '0' ]] ||
  fail 'a refused zero-count release must preserve the ownership marker for a retry'
# The remediation runs against prod, but an operator pastes it from whatever
# context their workstation currently has. Every mutation the guard performs
# itself pins admin@prod, so the commands it hands out must too, or the retry
# silently edits an identically named Deployment in another cluster and the
# production annotation stays at 0.
#
# Assert each command SEPARATELY. The refusal emits an `annotate` and a `scale`,
# so a single substring test over the combined output passes while either one of
# them is missing the flag — the other's flag satisfies it. Bound each match at
# `&` and `.` so it cannot run across the `&&` into its sibling command.
zero_count_annotate="$(
  printf '%s\n' "${zero_count_output}" |
    grep -o 'kubectl[^&.]*annotate deployment[^&.]*' || true
)"
zero_count_scale="$(
  printf '%s\n' "${zero_count_output}" |
    grep -o 'kubectl[^&.]*scale deployment[^&.]*' || true
)"
[[ "${zero_count_annotate}" == *'--context admin@prod'* ]] ||
  fail "the zero-count remediation's annotate command must pin the prod context; got: ${zero_count_annotate:-<no annotate command emitted>}"
[[ "${zero_count_scale}" == *'--context admin@prod'* ]] ||
  fail "the zero-count remediation's scale command must pin the prod context; got: ${zero_count_scale:-<no scale command emitted>}"

# The refusal has to cover EVERY release path, not just this phase. The normal
# deploy's always() post-deploy reassert invokes --after-deploy, and the DR
# workflow releases exclusively through it, so a refusal that guards only
# --after-revision-ready is undone moments later: restore_autoscaler_if_owned
# scales to the remembered zero and then DELETES the annotation, destroying the
# very marker this refusal just preserved.
printf '0\n' >"${state_dir}/replicas"
if zero_count_after_deploy="$(run_guard --after-deploy true 2>&1)"; then
  fail 'the post-deploy release path must also refuse a remembered zero replica count'
fi
[[ "${zero_count_after_deploy}" == *'remembered autoscaler count of 0'* ]] ||
  fail "the post-deploy zero-count refusal must name the conflict; got: ${zero_count_after_deploy}"
[[ "$(<"${state_dir}/previous-replicas")" == '0' ]] ||
  fail 'a refused post-deploy zero-count release must preserve the ownership marker'

# A ZERO-PADDED zero is the same conflict wearing a different spelling, and the
# annotation is operator-writable: the refusal above hands an operator an
# `annotate ... =<count>` command, so "00" is a plausible thing to arrive here.
# require_replica_count accepts it (^[0-9]+$), so a refusal that tests only the
# canonical "0" lets it through and does exactly the damage the refusal exists to
# prevent — scale the Deployment to zero, then DELETE the ownership annotation a
# retry needs. Assert the marker survives AND that nothing was scaled.
printf '00\n' >"${state_dir}/previous-replicas"
printf '0\n' >"${state_dir}/replicas"
if zero_padded_output="$(run_guard --after-revision-ready true 2>&1)"; then
  fail 'releasing a gate that owns a zero-padded remembered count must fail loudly'
fi
[[ "${zero_padded_output}" == *'remembered autoscaler count of 0'* ]] ||
  fail "the zero-padded refusal must name the conflict; got: ${zero_padded_output}"
[[ "$(<"${state_dir}/previous-replicas")" == '00' ]] ||
  fail 'a refused zero-padded release must preserve the ownership marker for a retry'
# The fake kubectl echoes back whatever it was scaled to, so a padded value that
# slipped through would leave "00" here rather than the "0" nothing-happened
# state. In prod the API canonicalises instead, and wait_for_replicas would then
# compare "00" against "0" and block until it times out.
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail "a refused zero-padded release must not scale the Deployment; got: $(<"${state_dir}/replicas")"

# An all-digit value past the shell's signed 64-bit range must be REFUSED, not
# silently wrapped. 18446744073709551617 is 2^64+1, which $(( )) folds to 1 — so
# normalising with arithmetic would "restore" one replica and delete both
# ownership annotations, which is the same destruction the zero refusal prevents,
# reached by a different route. spec.replicas is an int32, so this is out of
# range on its face.
printf '18446744073709551617\n' >"${state_dir}/previous-replicas"
printf '0\n' >"${state_dir}/replicas"
if overflow_output="$(run_guard --after-revision-ready true 2>&1)"; then
  fail 'releasing a gate that owns an out-of-range remembered count must fail loudly'
fi
[[ "${overflow_output}" == *'outside the Kubernetes replica range'* ]] ||
  fail "the out-of-range refusal must name the conflict; got: ${overflow_output}"
[[ "$(<"${state_dir}/previous-replicas")" == '18446744073709551617' ]] ||
  fail 'a refused out-of-range release must preserve the ownership marker for a retry'
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail "a refused out-of-range release must not scale the Deployment; got: $(<"${state_dir}/replicas")"

: >"${state_dir}/previous-replicas"

printf '0\n' >"${state_dir}/replicas"
run_guard --after-deploy true
[[ "$(<"${state_dir}/replicas")" == '0' ]] ||
  fail 'an unowned manual autoscaler suspension must remain untouched'

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
