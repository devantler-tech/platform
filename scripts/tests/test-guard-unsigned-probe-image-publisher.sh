#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/guard-unsigned-probe-image-publisher.sh.
#
# The guard fails by PASSING: if a check stops matching, a signing step can be
# added and CI stays green. So every violation case below asserts the specific
# message of the check it targets, not just a non-zero exit. A case that failed
# for some other reason would otherwise count as proof the guard works.
#
# Each case builds a fresh throwaway git repository holding copies of the REAL
# publisher and probe workflows, changes exactly one thing, and runs a copy of
# the guard against it. The unchanged fixture must pass first, so every failure
# is attributable to the one change.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard_rel='scripts/guard-unsigned-probe-image-publisher.sh'
readonly publisher_rel='.github/workflows/publish-unsigned-probe-image.yaml'
readonly probe_rel='.github/workflows/probe-image-signature-enforcement.yaml'
readonly image='ghcr.io/devantler-tech/unsigned-probe-throwaway'

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

ok() {
  pass_count=$((pass_count + 1))
  printf 'ok - %s\n' "$1"
}

for required in "$guard_rel" "$publisher_rel" "$probe_rel"; do
  [[ -f "${root_dir}/${required}" ]] || fail "missing ${required}"
done
command -v yq >/dev/null 2>&1 || fail 'yq is required'

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

fixture=''
output=''
rc=0

new_fixture() {
  fixture="$(mktemp -d "${work_dir}/fixture.XXXXXX")"
  mkdir -p "${fixture}/scripts" "${fixture}/.github/workflows"
  cp "${root_dir}/${guard_rel}" "${fixture}/${guard_rel}"
  cp "${root_dir}/${publisher_rel}" "${fixture}/${publisher_rel}"
  cp "${root_dir}/${probe_rel}" "${fixture}/${probe_rel}"
  git -C "${fixture}" init -q
}

run_guard() {
  git -C "${fixture}" add -A
  set +e
  output="$(bash "${fixture}/${guard_rel}" "${fixture}" 2>&1)"
  rc=$?
  set -e
}

# expect <description> <exit status> <text the output must contain>
expect() {
  local description="$1" want_rc="$2" want_text="$3"
  run_guard
  if [[ "${rc}" != "${want_rc}" ]]; then
    printf '%s\n' "${output}" >&2
    fail "${description}: exit ${rc}, expected ${want_rc}"
  fi
  if ! grep -qF -- "${want_text}" <<<"${output}"; then
    printf '%s\n' "${output}" >&2
    fail "${description}: output does not contain '${want_text}'"
  fi
  ok "${description}"
}

yq_edit() {
  yq -i "$1" "${fixture}/${publisher_rel}"
}

# Portable in-place substitution (BSD and GNU sed disagree on -i).
substitute() {
  local file="$1" expression="$2" tmp
  tmp="$(mktemp "${work_dir}/sub.XXXXXX")"
  sed -E "${expression}" "${file}" >"${tmp}"
  cat "${tmp}" >"${file}"
}

# --- Controls: what must pass ------------------------------------------------

set +e
real_output="$(bash "${root_dir}/${guard_rel}" "${root_dir}" 2>&1)"
real_rc=$?
set -e
if [[ "${real_rc}" != 0 ]]; then
  printf '%s\n' "${real_output}" >&2
  fail "the guard fails on this repository (exit ${real_rc})"
fi
ok 'the guard passes on this repository'

new_fixture
expect 'an unchanged fixture passes' 0 'unsigned probe image guard OK'

new_fixture
printf '# cosign sign, sigstore and attest are named here only in a comment\n' >>"${fixture}/${publisher_rel}"
expect 'a comment naming a signing tool passes' 0 'unsigned probe image guard OK'

# --- Violations: each must fail for its own reason ---------------------------

new_fixture
yq_edit '.jobs[].steps += [{"name": "Sign", "shell": "bash", "run": "cosign sign --yes ghcr.io/devantler-tech/unsigned-probe-throwaway:run-1-1"}]'
expect 'a cosign step is refused' 1 'names a signing or attestation tool'

new_fixture
yq_edit '.jobs[].steps += [{"name": "Attest", "shell": "bash", "run": "gh attestation verify oci://ghcr.io/devantler-tech/unsigned-probe-throwaway:run-1-1"}]'
expect 'an attestation step is refused' 1 'names a signing or attestation tool'

new_fixture
yq_edit '.jobs[].steps += [{"uses": "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6"}]'
expect 'a signing action is refused' 1 'the only allowed action is step-security/harden-runner'

new_fixture
yq_edit '.jobs[].steps += [{"uses": "docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a"}]'
expect 'any action other than harden-runner is refused' 1 'the only allowed action is step-security/harden-runner'

new_fixture
yq_edit '.jobs[].permissions["id-token"] = "write"'
expect 'a job-level id-token permission is refused' 1 'grants an id-token permission'

new_fixture
yq_edit '.permissions["id-token"] = "write"'
expect 'a workflow-level id-token permission is refused' 1 'grants an id-token permission'

new_fixture
yq_edit '.jobs[].permissions = "write-all"'
expect 'scalar write-all permissions are refused' 1 'sets permissions as a scalar'

new_fixture
yq_edit '.on.schedule = [{"cron": "0 0 * * *"}]'
expect 'a schedule trigger is refused' 1 'the only allowed trigger is workflow_dispatch'

new_fixture
yq_edit '.on.push = {"branches": ["main"]}'
expect 'a push trigger is refused' 1 'the only allowed trigger is workflow_dispatch'

new_fixture
yq_edit ".jobs[].steps[1].env.SIGNING_KEY = \"\${{ secrets.SIGNING_KEY }}\""
expect 'a secret other than GITHUB_TOKEN is refused' 1 'the only allowed secret is GITHUB_TOKEN'

new_fixture
substitute "${fixture}/${publisher_rel}" 's/--provenance=false/--provenance=mode=max/'
expect 'enabled provenance is refused' 1 'enables provenance or SBOM output'

new_fixture
yq_edit '.jobs[].env.IMAGE = "ghcr.io/devantler-tech/some-other-image"'
expect 'publishing a different image is refused' 1 'whose env.IMAGE is'

new_fixture
mkdir -p "${fixture}/k8s/bases/apps/demo"
printf 'apiVersion: v1\nkind: Pod\nmetadata:\n  name: demo\nspec:\n  containers:\n    - name: demo\n      image: %s:run-1-1\n' "${image}" \
  >"${fixture}/k8s/bases/apps/demo/pod.yaml"
expect 'a manifest referencing the image is refused' 1 'must stay referenced by no manifest'

new_fixture
substitute "${fixture}/${probe_rel}" '/unsigned-probe-throwaway/d'
expect 'a probe header that no longer names the image is refused' 1 'must document the negative-control ref'

new_fixture
printf 'name: Sign On Publish\non:\n  registry_package:\n    types: [published]\npermissions: {}\njobs:\n  noop:\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n' \
  >"${fixture}/.github/workflows/sign-on-publish.yaml"
expect 'a workflow triggered by package publishing is refused' 1 'runs on a package-publish event'

new_fixture
printf 'name: Sign On Publish\non: [registry_package]\npermissions: {}\njobs:\n  noop:\n    runs-on: ubuntu-latest\n    steps:\n      - run: "true"\n' \
  >"${fixture}/.github/workflows/sign-on-publish.yaml"
expect 'a list-form package-publish trigger is refused' 1 'runs on a package-publish event'

# --- Cannot check: never a pass -----------------------------------------------

new_fixture
rm "${fixture}/${publisher_rel}"
expect 'a missing publisher cannot be checked' 2 'cannot check'

# --- Wiring ------------------------------------------------------------------

ci="${root_dir}/.github/workflows/ci.yaml"
grep -qF "bash ${guard_rel} ." "${ci}" || fail 'ci.yaml does not run the guard'
grep -qF 'bash scripts/tests/test-guard-unsigned-probe-image-publisher.sh' "${ci}" || fail 'ci.yaml does not run this test'
ok 'ci.yaml runs the guard and this test'

printf '%d checks passed\n' "${pass_count}"
