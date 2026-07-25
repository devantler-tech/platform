#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly wait_script="${root_dir}/scripts/wait-for-platform-flux-revision.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"
readonly dr_workflow="${root_dir}/.github/workflows/dr-rebuild.yaml"
readonly dr_runbook="${root_dir}/docs/dr/runbook.md"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

line_of() {
  local file="$1"
  local needle="$2"
  grep -nF -- "${needle}" "${file}" | cut -d: -f1
}

[[ -x "${wait_script}" ]] ||
  fail 'the exact Flux revision wait must be an executable script'

deploy_reconcile_line="$(line_of "${deploy_action}" 'run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile')"
deploy_wait_line="$(line_of "${deploy_action}" "run: ./scripts/wait-for-platform-flux-revision.sh \"\${PLATFORM_MANIFEST_DIGEST}\"")" ||
  fail 'normal deploy must pass the published digest to the Flux wait through the environment'
deploy_cluster_update_line="$(line_of "${deploy_action}" 'run: ./scripts/run-ksail-prod-with-pull-auth.sh cluster update')"
readonly deploy_reconcile_line deploy_wait_line deploy_cluster_update_line
((deploy_reconcile_line < deploy_wait_line && deploy_wait_line < deploy_cluster_update_line)) ||
  fail 'normal deploys must observe the exact applied Flux revision before cluster update'
grep -Fq "steps.wait_flux_revision.outcome == 'success'" "${deploy_action}" ||
  fail 'cluster update and autoscaler release must require the exact Flux revision wait'
# GitHub evaluates this expression; the shell test intentionally matches it literally.
# shellcheck disable=SC2016
grep -Fq 'PLATFORM_MANIFEST_DIGEST: ${{ steps.cosign-sign.outputs.digest }}' "${deploy_action}" ||
  fail 'normal deploy must map the published digest into the Flux wait environment'
if grep -Fq -- '--revision-only' "${deploy_action}"; then
  fail 'normal deploy must require exact-revision readiness even while pods remain operator-stepped'
fi

dr_guard_lines="$(
  grep -nF './scripts/guard-cilium-homogeneous-device-rollout.sh' "${dr_workflow}" |
    cut -d: -f1
)"
readonly dr_guard_lines
dr_guard_count="$(printf '%s\n' "${dr_guard_lines}" | grep -c .)"
readonly dr_guard_count
[[ "${dr_guard_count}" -eq 2 ]] ||
  fail 'DR must invoke the rollout guard before publish and after convergence'
dr_guard_before_line="$(printf '%s\n' "${dr_guard_lines}" | sed -n '1p')"
dr_guard_after_line="$(printf '%s\n' "${dr_guard_lines}" | sed -n '2p')"
dr_push_line="$(line_of "${dr_workflow}" 'run: ./scripts/run-ksail-prod-with-pull-auth.sh workload push')"
dr_digest_line="$(line_of "${dr_workflow}" 'id: platform_manifest')"
dr_reconcile_line="$(line_of "${dr_workflow}" 'run: ./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile')"
dr_wait_line="$(line_of "${dr_workflow}" "run: ./scripts/wait-for-platform-flux-revision.sh \"\${PLATFORM_MANIFEST_DIGEST}\"")" ||
  fail 'DR must pass the published digest to the Flux wait through the environment'
dr_ready_line="$(grep -nFx '        id: wait_flux' "${dr_workflow}" | cut -d: -f1)"
readonly dr_guard_before_line dr_guard_after_line dr_push_line dr_digest_line dr_reconcile_line dr_wait_line
((dr_guard_before_line < dr_push_line)) ||
  fail 'DR must suspend autoscaling before publishing the active rollout gate'
((dr_push_line < dr_digest_line && dr_digest_line < dr_reconcile_line)) ||
  fail 'DR must resolve the newly published manifest digest before reconciliation'
((dr_reconcile_line < dr_wait_line && dr_wait_line < dr_ready_line && dr_ready_line < dr_guard_after_line)) ||
  fail 'DR must require exact revision readiness before full convergence and gate release'
# GitHub evaluates this expression; the shell test intentionally matches it literally.
# shellcheck disable=SC2016
grep -Fq 'PLATFORM_MANIFEST_DIGEST: ${{ steps.platform_manifest.outputs.digest }}' "${dr_workflow}" ||
  fail 'DR must map the published digest into the Flux wait environment'
if grep -Fq -- '--revision-only' "${dr_workflow}"; then
  fail 'DR must require exact-revision readiness while Cilium pods remain operator-stepped'
fi

manual_push_line="$(line_of "${dr_runbook}" './scripts/run-ksail-prod-with-pull-auth.sh workload push')"
# shellcheck disable=SC2016
manual_digest_line="$(line_of "${dr_runbook}" 'PLATFORM_MANIFEST_DIGEST="$(docker buildx imagetools inspect')" ||
  fail 'manual DR must resolve the newly published platform digest'
manual_reconcile_line="$(line_of "${dr_runbook}" './scripts/run-ksail-prod-with-pull-auth.sh workload reconcile')"
# shellcheck disable=SC2016
manual_wait_line="$(line_of "${dr_runbook}" './scripts/wait-for-platform-flux-revision.sh "${PLATFORM_MANIFEST_DIGEST}"')" ||
  fail 'manual DR must wait for the exact published platform revision'
manual_guard_after_line="$(line_of "${dr_runbook}" './scripts/guard-cilium-homogeneous-device-rollout.sh --after-deploy')"
readonly manual_push_line manual_digest_line manual_reconcile_line manual_wait_line manual_guard_after_line

((manual_push_line < manual_digest_line && manual_digest_line < manual_reconcile_line)) ||
  fail 'manual DR must resolve the newly published manifest digest before reconciliation'
((manual_reconcile_line < manual_wait_line && manual_wait_line < manual_guard_after_line)) ||
  fail 'manual DR must prove the exact published revision is Ready before releasing autoscaling'
# shellcheck disable=SC2016
grep -Fq '[[ "${PLATFORM_MANIFEST_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]]' "${dr_runbook}" ||
  fail 'manual DR must validate the resolved platform digest before using it'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf -- "${tmp_dir}"' EXIT

fake_kubectl="${tmp_dir}/kubectl"
commands="${tmp_dir}/commands"
: >"${commands}"
cat >"${fake_kubectl}" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${KUBECTL_COMMANDS}"
FAKE_KUBECTL
chmod +x "${fake_kubectl}"

readonly expected_digest='sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
KUBECTL="${fake_kubectl}" \
  KUBECTL_COMMANDS="${commands}" \
  "${wait_script}" "${expected_digest}"

[[ "$(wc -l <"${commands}" | tr -d ' ')" == '2' ]] ||
  fail 'the Flux wait must verify both the applied revision and Ready condition'
grep -Fq -- '--context admin@prod -n flux-system wait kustomization/infrastructure-controllers --for=jsonpath={.status.lastAppliedRevision}=latest@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef --timeout=20m' "${commands}" ||
  fail 'the Flux wait must pin the newly published infrastructure-controllers revision in prod'
grep -Fq -- '--context admin@prod -n flux-system wait kustomization/infrastructure-controllers --for=condition=Ready --timeout=20m' "${commands}" ||
  fail 'the exact applied revision must also be Ready before autoscaling resumes'

: >"${commands}"
if KUBECTL="${fake_kubectl}" \
  KUBECTL_COMMANDS="${commands}" \
  "${wait_script}" 'latest'; then
  fail 'an unpinned Flux revision must fail closed'
fi
[[ ! -s "${commands}" ]] ||
  fail 'an invalid revision must be rejected before querying the cluster'

printf 'PASS: both prod delivery paths wait for the exact applied Flux revision before autoscaling resumes\n'
