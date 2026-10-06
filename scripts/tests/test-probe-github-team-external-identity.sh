#!/usr/bin/env bash
# Pin the behaviour of scripts/probe-github-team-external-identity.sh.
#
# WHY THIS EXISTS. The probe reports whether production admission enforces a security control
# (#3144), and both of its possible mistakes are silent: ENFORCED where a refusal came from RBAC,
# another policy or a dead connection, and NOT-ENFORCED where the answer was merely unreadable.
# So each conclusive verdict has a control that differs in one answer, and every INCONCLUSIVE path
# is exercised on purpose.
#
# It also pins what makes the workflow safe to dispatch with a production credential: the probe
# issues only `get`, and `patch`/`create` that carry `--dry-run=server`, and it prints no identity,
# ServiceAccount name or address into a public log.
#
# kubectl is faked from a fixture directory; no cluster, no secrets, no network. Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/probe-github-team-external-identity.sh"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

readonly fake_bin="${work_dir}/bin"
readonly fixtures="${work_dir}/fixtures"
mkdir -p "${fake_bin}"

fail() {
  printf 'FAIL: %s\n--- actual output (rc=%s) ---\n%s\n---\n' "$1" "${rc:-?}" "${output:-}" >&2
  exit 1
}

require_text() {
  grep -Fq -- "$1" <<<"${output}" || fail "$2"
}

refute_text() {
  if grep -Fq -- "$1" <<<"${output}"; then
    fail "$2"
  fi
}

require_rc() {
  [[ "${rc}" -eq "$1" ]] || fail "$2 (expected rc=$1)"
}

# ---------------------------------------------------------------------------
# Fake kubectl. Records every invocation and answers the way a cluster with a working policy
# does: the provider may write any identity, anyone else only the identity the Team already has.
# A file named after a probe class in ${FIXTURES}/answers overrides that class with one of
# admitted | refused | exists | forbidden | other-policy | connection.
# ---------------------------------------------------------------------------
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/calls.log"
if [[ "${1:-}" != --context || "${2:-}" != admin@prod ]]; then
  touch "${FIXTURES}/UNEXPECTED_CONTEXT"
  exit 1
fi
shift 2
verb="$1"
shift
connection_error() {
  printf 'error: dial tcp 198.51.100.9:6443: connect: connection refused\n' >&2
  exit 1
}
if [[ "${verb}" == get ]]; then
  case "$1" in
    clusterpolicy) file=policy.json ;;
    teams.team.github.m.upbound.io) file=teams.json ;;
    pods) file=pods.json ;;
    *) touch "${FIXTURES}/UNEXPECTED_READ"; exit 1 ;;
  esac
  [[ -f "${FIXTURES}/${file}" ]] || connection_error
  cat "${FIXTURES}/${file}"
  exit 0
fi
if [[ "${verb}" != patch && "${verb}" != create ]]; then
  touch "${FIXTURES}/UNEXPECTED_VERB"
  exit 1
fi
as='' team='' identity='' dry=no
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --as) as="$2"; shift 2 ;;
    --dry-run=server) dry=yes; shift ;;
    --patch)
      identity="$(jq -r '.metadata.annotations["crossplane.io/external-name"]' <<<"$2")"
      shift 2 ;;
    --filename)
      team="$(jq -r '.metadata.name' "$2")"
      identity="$(jq -r '.metadata.annotations["crossplane.io/external-name"]' "$2")"
      jq -e '.spec and .kind == "Team" and .metadata.namespace == "github-config"
             and (.status | not) and (.metadata.uid | not)' "$2" >/dev/null ||
        touch "${FIXTURES}/BAD_MANIFEST"
      shift 2 ;;
    --namespace | --type) shift 2 ;;
    teams.team.github.m.upbound.io) team="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [[ "${dry}" != yes ]]; then
  touch "${FIXTURES}/WRITE_WITHOUT_DRY_RUN"
  exit 1
fi
current="$(jq -r --arg name "${team}" \
  '.items[] | select(.metadata.name == $name)
   | .metadata.annotations["crossplane.io/external-name"]' "${FIXTURES}/teams.json")"
if [[ "${as}" == system:serviceaccount:crossplane-system:* ]]; then
  class=provider
  answer=admitted
elif [[ "${as}" != system:serviceaccount:github-config:github-config ]]; then
  touch "${FIXTURES}/UNEXPECTED_IDENTITY"
  exit 1
elif [[ "${identity}" == "${current}" ]]; then
  if [[ "${verb}" == create ]]; then class=adoption; answer=exists; else class=unchanged; answer=admitted; fi
else
  if [[ "${verb}" == create ]]; then
    class=re-create
  elif jq -e --arg id "${identity}" --arg key crossplane.io/external-name \
    'any(.items[]; .metadata.annotations[$key] == $id)' "${FIXTURES}/teams.json" >/dev/null; then
    class=swapped
  else
    class=foreign
  fi
  answer=refused
fi
if [[ -f "${FIXTURES}/answers/${class}" ]]; then
  answer="$(cat "${FIXTURES}/answers/${class}")"
fi
case "${answer}" in
  admitted) exit 0 ;;
  refused)
    printf 'Error from server: admission webhook "validate.kyverno.svc-fail" denied the request: resource Team/github-config/%s was blocked due to the following policies restrict-github-team-external-identity: teams-external-name-is-an-approved-identity: denied\n' "${team}" >&2
    exit 1 ;;
  exists)
    printf 'Error from server (AlreadyExists): teams.team.github.m.upbound.io "%s" already exists\n' "${team}" >&2
    exit 1 ;;
  forbidden)
    printf 'Error from server (Forbidden): teams.team.github.m.upbound.io "%s" is forbidden: User cannot patch resource\n' "${team}" >&2
    exit 1 ;;
  other-policy)
    printf 'Error from server: admission webhook "validate.kyverno.svc-fail" denied the request: resource Team/github-config/%s was blocked due to the following policies restrict-github-team-management: teams-are-allow-listed: denied\n' "${team}" >&2
    exit 1 ;;
  *) connection_error ;;
esac
FAKE
chmod +x "${fake_bin}/kubectl"

readonly admins_identity='7000001'
readonly maintainers_identity='7000002'
readonly provider_account='provider-upjet-github-0123456789ab'

reset_fixtures() {
  rm -rf "${fixtures}"
  mkdir -p "${fixtures}/answers"
  cat >"${fixtures}/policy.json" <<'JSON'
{"kind":"ClusterPolicy","spec":{"rules":[{"name":"teams-external-name-is-an-approved-identity",
 "validate":{"failureAction":"Enforce","deny":{"conditions":{"all":[{"key":
 "{{ contains(['admins/7000001', 'maintainers/7000002'], join('/', [name, externalName])) }}"}]}}}}]},
 "status":{"conditions":[{"type":"Ready","status":"True"}]}}
JSON
  jq -n --arg a "${admins_identity}" --arg m "${maintainers_identity}" '
    def team($name; $id): {apiVersion: "team.github.m.upbound.io/v1alpha1", kind: "Team",
      metadata: {name: $name, namespace: "github-config", uid: ("uid-" + $name),
                 annotations: {"crossplane.io/external-name": $id}},
      spec: {forProvider: {name: $name}}, status: {atProvider: {}}};
    {kind: "List", items: [team("admins"; $a), team("maintainers"; $m)]}' >"${fixtures}/teams.json"
  jq -n --arg sa "${provider_account}" '
    {kind: "List", items: [
      {metadata: {name: "provider-pod"}, spec: {serviceAccountName: $sa}, status: {phase: "Running"}},
      {metadata: {name: "crossplane"}, spec: {serviceAccountName: "crossplane"}, status: {phase: "Running"}}]}' \
    >"${fixtures}/pods.json"
}

rc=0
output=''
run_probe() {
  rc=0
  output="$(FIXTURES="${fixtures}" PATH="${fake_bin}:${PATH}" GITHUB_STEP_SUMMARY="${fixtures}/summary.md" \
    bash "${script}" "$@" 2>&1)" || rc=$?
}

# Holds for every run that reached the cluster, whatever the verdict.
require_safe_run() {
  local marker
  for marker in UNEXPECTED_CONTEXT UNEXPECTED_READ UNEXPECTED_VERB UNEXPECTED_IDENTITY \
    WRITE_WITHOUT_DRY_RUN BAD_MANIFEST; do
    [[ ! -e "${fixtures}/${marker}" ]] || fail "$1: the probe tripped ${marker}"
  done
  refute_text "${admins_identity}" "$1: an identity reached the log"
  refute_text "${maintainers_identity}" "$1: an identity reached the log"
  refute_text "${provider_account}" "$1: the provider ServiceAccount name reached the log"
  refute_text '198.51.100.9' "$1: an address reached the log"
  if [[ -f "${fixtures}/summary.md" ]] &&
    grep -Eq "${admins_identity}|${maintainers_identity}|${provider_account}|198\.51\.100\.9" "${fixtures}/summary.md"; then
    fail "$1: an identity, ServiceAccount name or address reached the step summary"
  fi
}

cases_run=0
case_done() {
  cases_run=$((cases_run + 1))
  printf '  ok  %s\n' "$1"
}

printf 'test-probe-github-team-external-identity\n'

# --- usage -----------------------------------------------------------------
reset_fixtures
run_probe
require_rc 1 'no arguments is a usage error'
[[ ! -e "${fixtures}/calls.log" ]] || fail 'a usage error must not reach the cluster'
run_probe --context
require_rc 1 'a context flag without a value is a usage error'
run_probe --context admin@prod --apply
require_rc 1 'an unknown flag is a usage error'
[[ ! -e "${fixtures}/calls.log" ]] || fail 'a usage error must not reach the cluster'
case_done 'usage errors exit 1 before any call'

# --- ENFORCED ---------------------------------------------------------------
reset_fixtures
run_probe --context admin@prod
require_rc 0 'a working policy is ENFORCED'
require_text 'ENFORCED: production admission refused every foreign identity' 'the verdict line is missing'
grep -Fq 'ENFORCED: production admission refused' "${fixtures}/summary.md" || fail 'the verdict must reach the step summary'
require_text 'for 2 Team object(s)' 'the verdict must say how many Teams were probed'
require_safe_run 'ENFORCED'
# 2 Teams x (unchanged, foreign, swapped, provider) patches and x (re-create, adoption) creates.
[[ "$(grep -c ' patch ' "${fixtures}/calls.log")" -eq 8 ]] || fail 'expected 8 dry-run patches'
[[ "$(grep -c ' create ' "${fixtures}/calls.log")" -eq 4 ]] || fail 'expected 4 dry-run creates'
[[ "$(grep -c -- '--dry-run=server' "${fixtures}/calls.log")" -eq 12 ]] ||
  fail 'every write must carry --dry-run=server'
[[ "$(grep -c "system:serviceaccount:crossplane-system:${provider_account}" "${fixtures}/calls.log")" -eq 2 ]] ||
  fail 'the provider probe must run as the ServiceAccount read from the running pod'
if grep -Ev '^--context admin@prod (get|patch|create) ' "${fixtures}/calls.log" >/dev/null; then
  fail 'only get, patch and create may be issued'
fi
case_done 'a working policy is ENFORCED, with dry-run writes only'

reset_fixtures
jq '.items |= .[0:1]' "${fixtures}/teams.json" >"${fixtures}/one.json"
mv "${fixtures}/one.json" "${fixtures}/teams.json"
run_probe --context admin@prod
require_rc 0 'a single Team is still ENFORCED (there is nothing to swap with)'
refute_text 'swapped' 'a single Team has no swapped probe'
require_safe_run 'single Team'
case_done 'a single Team skips the swapped probe'

# --- NOT-ENFORCED: one answer differs from the ENFORCED fixture ---------------
for class in foreign swapped re-create; do
  reset_fixtures
  printf 'admitted' >"${fixtures}/answers/${class}"
  run_probe --context admin@prod
  require_rc 2 "an admitted ${class} identity is NOT-ENFORCED"
  require_text 'NOT-ENFORCED' "an admitted ${class} identity must be named NOT-ENFORCED"
  refute_text 'ENFORCED: production admission refused' "an admitted ${class} identity must not read ENFORCED"
  require_safe_run "admitted ${class}"
  case_done "an admitted ${class} identity is NOT-ENFORCED"
done

for class in unchanged adoption provider; do
  reset_fixtures
  printf 'refused' >"${fixtures}/answers/${class}"
  run_probe --context admin@prod
  require_rc 2 "a legitimate ${class} write refused by this policy is NOT-ENFORCED"
  require_safe_run "refused ${class}"
  case_done "a ${class} write refused by this policy is NOT-ENFORCED"
done

# --- INCONCLUSIVE: an answer that does not name the policy --------------------
for answer in forbidden other-policy connection; do
  reset_fixtures
  printf '%s' "${answer}" >"${fixtures}/answers/foreign"
  run_probe --context admin@prod
  require_rc 3 "a foreign write answered with ${answer} is INCONCLUSIVE"
  require_text 'INCONCLUSIVE' "${answer} must be named INCONCLUSIVE"
  refute_text 'ENFORCED: production admission refused' "${answer} must not read ENFORCED"
  require_safe_run "foreign ${answer}"
  case_done "a foreign write answered with ${answer} is INCONCLUSIVE, not a refusal"
done

reset_fixtures
printf 'forbidden' >"${fixtures}/answers/unchanged"
run_probe --context admin@prod
require_rc 3 'a legitimate write refused by RBAC is INCONCLUSIVE, not a policy defect'
require_safe_run 'unchanged forbidden'
case_done 'a legitimate write refused by RBAC is INCONCLUSIVE'

reset_fixtures
printf 'admitted' >"${fixtures}/answers/adoption"
run_probe --context admin@prod
require_rc 3 'an adoption that is stored outright means the object vanished: INCONCLUSIVE'
require_safe_run 'adoption admitted'
case_done 'an adoption admitted outright is INCONCLUSIVE'

# A wrong answer outranks an unattributed one: the defect must not hide behind the noise.
reset_fixtures
printf 'admitted' >"${fixtures}/answers/foreign"
printf 'connection' >"${fixtures}/answers/provider"
run_probe --context admin@prod
require_rc 2 'a wrong answer beside an unattributed one is still NOT-ENFORCED'
case_done 'a wrong answer outranks an unattributed one'

# --- INCONCLUSIVE: preconditions ---------------------------------------------
precondition() {
  run_probe --context admin@prod
  require_rc 3 "$1 is INCONCLUSIVE"
  require_text 'INCONCLUSIVE' "$1 must be named INCONCLUSIVE"
  if grep -Eq ' (patch|create) ' "${fixtures}/calls.log" 2>/dev/null; then
    fail "$1: no probe may be sent"
  fi
  require_safe_run "$1"
  case_done "$1 is INCONCLUSIVE before any probe"
}

reset_fixtures
rm "${fixtures}/policy.json"
precondition 'an unreadable policy'

reset_fixtures
jq '.status.conditions[0].status = "False"' "${fixtures}/policy.json" >"${fixtures}/p.json"
mv "${fixtures}/p.json" "${fixtures}/policy.json"
precondition 'a policy that is not Ready'

reset_fixtures
jq '.spec.rules[0].validate.failureAction = "Audit"' "${fixtures}/policy.json" >"${fixtures}/p.json"
mv "${fixtures}/p.json" "${fixtures}/policy.json"
precondition 'a policy in Audit'

reset_fixtures
printf 'not json' >"${fixtures}/policy.json"
precondition 'a policy read that is not JSON'

reset_fixtures
rm "${fixtures}/teams.json"
precondition 'an unreadable Team list'

reset_fixtures
jq '.items = []' "${fixtures}/teams.json" >"${fixtures}/t.json"
mv "${fixtures}/t.json" "${fixtures}/teams.json"
precondition 'an empty Team list'

reset_fixtures
jq '.items[1].metadata.annotations = {}' "${fixtures}/teams.json" >"${fixtures}/t.json"
mv "${fixtures}/t.json" "${fixtures}/teams.json"
precondition 'a Team without an identity'

reset_fixtures
jq '.items[0].metadata.annotations["crossplane.io/external-name"] = "*"' \
  "${fixtures}/teams.json" >"${fixtures}/t.json"
mv "${fixtures}/t.json" "${fixtures}/teams.json"
precondition 'a Team whose identity is not a number'

# The provider created the Team anew: the policy refuses its re-adoption by design until a
# reviewed change lists the new identity, so the probe must not call that a defect.
reset_fixtures
jq '.items[0].metadata.annotations["crossplane.io/external-name"] = "7000009"' \
  "${fixtures}/teams.json" >"${fixtures}/t.json"
mv "${fixtures}/t.json" "${fixtures}/teams.json"
precondition 'a Team whose identity the policy does not list'

reset_fixtures
rm "${fixtures}/pods.json"
precondition 'an unreadable provider pod list'

reset_fixtures
jq '.items[0].status.phase = "Pending"' "${fixtures}/pods.json" >"${fixtures}/p.json"
mv "${fixtures}/p.json" "${fixtures}/pods.json"
precondition 'no running provider pod'

reset_fixtures
jq '.items += [.items[0] | .spec.serviceAccountName = "provider-upjet-github-ffffffffffff"]' \
  "${fixtures}/pods.json" >"${fixtures}/p.json"
mv "${fixtures}/p.json" "${fixtures}/pods.json"
precondition 'two provider ServiceAccounts'

# --- workflow shape -----------------------------------------------------------
# The script is only as safe as the way it is dispatched, so the workflow's shape is pinned too.
readonly workflow="${root_dir}/.github/workflows/probe-github-team-external-identity.yaml"
wf_fail() {
  printf 'FAIL: workflow contract: %s\n' "$1" >&2
  exit 1
}
wf_line() {
  { grep -n -F -- "$1" "${workflow}" || true; } | head -n 1 | cut -d: -f1
}
[[ -f "${workflow}" ]] || wf_fail 'the workflow file is missing'
if grep -Eq '^[[:space:]]+(schedule|push|pull_request|pull_request_target|merge_group):' "${workflow}"; then
  wf_fail 'the workflow must stay dispatch-only'
fi
grep -Eq '^permissions: \{\}$' "${workflow}" || wf_fail 'top-level permissions must be empty'
[[ "$(grep -Ec '^[[:space:]]+[a-z-]+: (read|write)( |$)' "${workflow}")" -eq 1 ]] ||
  wf_fail 'the job may hold exactly one permission'
grep -Eq '^      contents: read( |$)' "${workflow}" || wf_fail 'the job permission must be contents: read'
grep -Eq '^  group: prod-deploy$' "${workflow}" || wf_fail 'concurrency must serialise on prod-deploy'
grep -Eq '^  cancel-in-progress: false$' "${workflow}" || wf_fail 'cancel-in-progress must be false'
main_guard="$(wf_line "!= 'refs/heads/main'")"
checkout="$(wf_line 'uses: actions/checkout@')"
confirm="$(wf_line "!= 'probe-production-team-admission'")"
restore="$(wf_line 'secrets.KUBE_CONFIG')"
endpoint="$(wf_line 'run: ./scripts/use-prod-stable-api-endpoint.sh >/dev/null')"
probe="$(wf_line 'run: ./scripts/probe-github-team-external-identity.sh --context admin@prod')"
for step in "${main_guard}" "${checkout}" "${confirm}" "${restore}" "${endpoint}" "${probe}"; do
  [[ -n "${step}" ]] || wf_fail 'a required step is missing'
done
[[ "${main_guard}" -lt "${checkout}" && "${checkout}" -lt "${confirm}" &&
  "${confirm}" -lt "${restore}" && "${restore}" -lt "${endpoint}" && "${endpoint}" -lt "${probe}" ]] ||
  wf_fail 'steps must run as: main guard, checkout, confirmation, kubeconfig, endpoint, probe'
readonly dollar='$'
grep -Fq "ref: ${dollar}{{ github.sha }}" "${workflow}" || wf_fail 'checkout must be pinned to the dispatch commit'
[[ "$(grep -c 'secrets\.' "${workflow}")" -eq 2 ]] ||
  wf_fail 'only KUBE_CONFIG and HCLOUD_TOKEN may be read, once each'
if grep -Fq "${dollar}{{ inputs." <(grep -v '^[[:space:]]*CONFIRM: ' "${workflow}"); then
  wf_fail 'the input may only reach bash through env'
fi
case_done 'the workflow is dispatch-only, main-only, confirmed and serialised with deploys'

printf '\nAll %d case(s) passed: probe-github-team-external-identity.sh behaviour is pinned.\n' "${cases_run}"
