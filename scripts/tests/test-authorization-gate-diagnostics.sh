#!/usr/bin/env bash
# Prove the authorization gate reports BOTH of its outputs on a real mismatch
# (devantler-tech/platform#3879).
#
# WHY THIS EXISTS. The "🔐 Validate static production authorization controls"
# step runs two independent commands over the same surface:
#
#   * `go test ./scripts/validate-eks-ci-role-policy` — the verdict. On an
#     unapproved aggregate TestValidateAuthorizationAcceptsCommittedPolicy
#     t.Fatalf's with the bare fingerprint.
#   * `go run ./scripts/validate-eks-ci-role-policy . "$base_root"` — the
#     diagnostics. This is the ONLY caller of surfaceMismatchReport, the code
#     #3836 added to name the surface entries that moved.
#
# A real aggregate mismatch fails BOTH. So a step that short-circuits on the
# first command never reaches the second, and the failure that most needs an
# explanation is the one reported with no entry names at all — measured on
# #3878 (CI run 35257229458, job 105324573289): the approval base checked out
# fine, `go test` failed with `a4781e58…`, and zero moved-entry lines were
# emitted.
#
# The step's script is EXECUTED here against stub `go` and `git` binaries rather
# than pattern-matched, so the assertions are about behaviour: which commands
# ran, and what the step exited with. Every assertion is ABLATED against the
# short-circuiting form the fix replaces; a check that cannot fail is not a
# check.
#
# yq (mikefarah v4) reads the YAML. No network, no secrets, no Go toolchain.
# Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly job='validate-eks-authorization'
readonly step='🔐 Validate static production authorization controls'
readonly diagnostic_line='changed helm.toolkit.fluxcd.io/v2|HelmRelease|kube-system|hcloud-csi'

work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

failures=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

# extract_step prints the step's `run:` script for one workflow.
extract_step() {
  local workflow="$1"
  yq -r \
    ".jobs[\"${job}\"].steps[] | select(.name == \"${step}\") | .run" \
    "${workflow}"
}

# step_shell_flags prints the flags the runner gives the step's interpreter, so
# the script is exercised the way GitHub actually runs it rather than under a
# bare `bash`. A step with no `shell:` key gets `bash -e {0}`; an explicit
# `shell: bash` gets `bash --noprofile --norc -eo pipefail {0}`. The difference
# decides whether a failing first command aborts the step, which is the very
# property under test — running these under a plain `bash` reports a swallowed
# failure that the runner would never produce.
step_shell_flags() {
  local workflow="$1" declared
  declared="$(yq -r \
    ".jobs[\"${job}\"].steps[] | select(.name == \"${step}\") | .shell // \"\"" \
    "${workflow}")"
  case "${declared}" in
    bash) printf -- '-e -o pipefail' ;;
    '') printf -- '-e' ;;
    *) printf -- '-e' ;;
  esac
}

# make_stubs builds a PATH directory whose `go` records each invocation and
# exits with the caller's chosen status, and whose `git` always succeeds so the
# approval-base branch is taken.
#
#   $1 work root   $2 `go test` exit status   $3 `go run` exit status
make_stubs() {
  local dir="$1" test_status="$2" run_status="$3"
  mkdir -p "${dir}/bin"
  cat >"${dir}/bin/go" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$1" >>"${dir}/invocations"
case "\$1" in
  test) printf 'FAIL\tgithub.com/devantler-tech/platform/scripts/validate-eks-ci-role-policy\n'; exit ${test_status} ;;
  run)  printf 'EKS CI role policy: ${diagnostic_line}\n'; exit ${run_status} ;;
esac
exit 0
EOF
  cat >"${dir}/bin/git" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "${dir}/bin/go" "${dir}/bin/git"
  : >"${dir}/invocations"
}

# run_step executes a step script under the stubs and prints
# "<exit status>|<space-separated go subcommands>|<stdout+stderr on one line>".
#
#   $1 script path   $2 `go test` exit status   $3 `go run` exit status
#   $4 the runner's shell flags for that step
run_step() {
  local script="$1" test_status="$2" run_status="$3" shell_flags="$4"
  local sandbox status output
  sandbox="$(mktemp -d "${work_dir}/sandbox.XXXXXX")"
  make_stubs "${sandbox}" "${test_status}" "${run_status}"
  set +e
  # shellcheck disable=SC2086 # shell_flags is a controlled flag list, not a path.
  output="$(PATH="${sandbox}/bin:${PATH}" RUNNER_TEMP="${sandbox}" \
    bash ${shell_flags} "${script}" 2>&1)"
  status=$?
  set -e
  printf '%s|%s|%s\n' \
    "${status}" \
    "$(tr '\n' ' ' <"${sandbox}/invocations" | sed 's/ *$//')" \
    "$(printf '%s' "${output}" | tr '\n' ' ')"
}

# field reads one of run_step's three fields. The captured output is LAST and is
# read with an open-ended range, because a surface entry is itself pipe-separated
# (`apiVersion|Kind|namespace|name`) — a fixed `-f3` truncates the diagnostics at
# their first separator and reports a line that was printed as missing.
field() {
  case "$2" in
    3) printf '%s' "$1" | cut -d'|' -f3- ;;
    *) printf '%s' "$1" | cut -d'|' -f"$2" ;;
  esac
}

# assert_gate_behaviour runs the three cases every copy of the step must satisfy.
#
#   $1 label   $2 script path   $3 shell flags
#   $4 "diagnostics" to require the moved-entry line
assert_gate_behaviour() {
  local label="$1" script="$2" shell_flags="$3" mode="${4:-}"
  local result

  # Case A — a real aggregate mismatch: both commands fail. The step must run
  # the diagnostics anyway, and must still fail.
  result="$(run_step "${script}" 1 1 "${shell_flags}")"
  case " $(field "${result}" 2) " in
    *' run '*) : ;;
    *) fail "${label}: a failing 'go test' short-circuited the step before 'go run' — the moved-entry diagnostics never execute on the one failure they exist to explain (got invocations: '$(field "${result}" 2)')" ;;
  esac
  [ "$(field "${result}" 1)" != "0" ] ||
    fail "${label}: the step exited 0 while both commands failed"
  if [ "${mode}" = "diagnostics" ]; then
    case "$(field "${result}" 3)" in
      *"${diagnostic_line}"*) : ;;
      *) fail "${label}: the moved-entry diagnostics were not reported (got: '$(field "${result}" 3)')" ;;
    esac
  fi

  # Case B — negative control: `go test` fails for a reason unrelated to the
  # aggregate, so `go run` passes. Running both must not swallow the failure.
  result="$(run_step "${script}" 1 0 "${shell_flags}")"
  [ "$(field "${result}" 1)" != "0" ] ||
    fail "${label}: a failing 'go test' was swallowed when 'go run' passed — the step reports success on a broken validator"

  # Case C — control: both pass, so the step passes.
  result="$(run_step "${script}" 0 0 "${shell_flags}")"
  [ "$(field "${result}" 1)" = "0" ] ||
    fail "${label}: the step failed while both commands passed (exit $(field "${result}" 1): '$(field "${result}" 3)')"
}

for workflow_name in ci cd; do
  workflow="${root_dir}/.github/workflows/${workflow_name}.yaml"
  [ -f "${workflow}" ] || { fail "${workflow_name}.yaml is missing"; continue; }

  script="${work_dir}/${workflow_name}-step.sh"
  extract_step "${workflow}" >"${script}"
  shell_flags="$(step_shell_flags "${workflow}")"
  if [ ! -s "${script}" ] || grep -qx 'null' "${script}"; then
    fail "${workflow_name}.yaml: job '${job}' has no step named '${step}' — the gate this test pins was renamed or removed"
    continue
  fi
  grep -q 'validate-eks-ci-role-policy' "${script}" ||
    fail "${workflow_name}.yaml: the extracted step does not invoke the authorization validator"

  # ci.yaml checks out the approval base, so its diagnostics name the entries.
  if grep -q 'approval-base-tree' "${script}"; then
    assert_gate_behaviour "${workflow_name}.yaml" "${script}" "${shell_flags}" diagnostics
  else
    assert_gate_behaviour "${workflow_name}.yaml" "${script}" "${shell_flags}"
  fi
done

# ABLATION. The short-circuiting form this fix replaces must fail Case A, and
# must fail naming the short-circuit rather than something incidental. A test
# that passes against the defect proves nothing.
ablation="${work_dir}/ablation-step.sh"
cat >"${ablation}" <<'EOF'
set -euo pipefail
go test ./scripts/validate-eks-ci-role-policy
base_root="$RUNNER_TEMP/approval-base-tree"
if git worktree add --detach "$base_root" HEAD^1; then
  go run ./scripts/validate-eks-ci-role-policy . "$base_root"
else
  go run ./scripts/validate-eks-ci-role-policy .
fi
EOF
ablation_result="$(run_step "${ablation}" 1 1 '-e')"
case " $(field "${ablation_result}" 2) " in
  *' run '*)
    fail "ABLATION: the short-circuiting step reached 'go run', so Case A cannot detect the defect it is written for"
    ;;
  *) : ;;
esac
[ "$(field "${ablation_result}" 1)" != "0" ] ||
  fail "ABLATION: the short-circuiting step exited 0, so Case B's control is not exercised"

if [ "${failures}" -ne 0 ]; then
  printf '%s: %d assertion(s) failed\n' "$(basename "$0")" "${failures}" >&2
  exit 1
fi

printf '%s: authorization gate reports its verdict AND its diagnostics in ci.yaml and cd.yaml\n' \
  "$(basename "$0")"
