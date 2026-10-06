#!/usr/bin/env bash
# Pin the behaviour of scripts/read-actual-budget-userns-maps.sh.
#
# The script runs against production with a cluster-admin credential, and only on dispatch, so
# nothing but this test exercises it before it is used. What makes dispatching it acceptable is
# pinned here against a fake kubectl:
#   * it issues one `get pods` and four `exec … -- cat /proc/self/{uid,gid}_map`, and nothing else;
#   * the exec command line is fixed, in the expected namespace, pod and containers;
#   * each verdict is reached from a one-fixture change against a passing control;
#   * a read that failed or could not be judged is INCONCLUSIVE, never MAPPED;
#   * no node-side id, pod name or address reaches the public log.
#
# Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/read-actual-budget-userns-maps.sh"

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
# Fake kubectl. Records every invocation. `get pods` answers from pods.json; `exec` answers from
# maps/<container>.<map>, and only for the exact command line the script is allowed to send.
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
connection_error() {
  printf 'error: dial tcp 198.51.100.9:6443: connect: connection refused\n' >&2
  exit 1
}
case "$*" in
  'get pods --namespace actual-budget --selector app.kubernetes.io/name=actualbudget --output json')
    [[ -f "${FIXTURES}/pods.json" ]] || connection_error
    cat "${FIXTURES}/pods.json"
    exit 0
    ;;
esac
if [[ "${1:-}" != exec ]]; then
  touch "${FIXTURES}/UNEXPECTED_VERB"
  exit 1
fi
if [[ "$#" -ne 9 || "$2" != --namespace || "$3" != actual-budget || "$4" != "${EXPECTED_POD}" ||
  "$5" != --container || "$7" != -- || "$8" != cat ]]; then
  touch "${FIXTURES}/UNEXPECTED_EXEC"
  exit 1
fi
case "$6" in
  actualbudget | enablebanking-seed) ;;
  *) touch "${FIXTURES}/UNEXPECTED_EXEC"; exit 1 ;;
esac
case "$9" in
  /proc/self/uid_map | /proc/self/gid_map) ;;
  *) touch "${FIXTURES}/UNEXPECTED_EXEC"; exit 1 ;;
esac
file="${FIXTURES}/maps/$6.${9##*/}"
[[ -f "${file}" ]] || connection_error
cat "${file}"
FAKE
chmod +x "${fake_bin}/kubectl"

readonly pod_name='actual-budget-7c9d8b6f5d-x2k4q'
# The node-side ids. Neither may reach the log.
readonly uid_base='2147811328'
readonly gid_base='3221291008'

write_map() {
  printf '%s' "$2" >"${fixtures}/maps/$1"
}

reset_fixtures() {
  rm -rf "${fixtures}"
  mkdir -p "${fixtures}/maps"
  jq -n --arg pod "${pod_name}" '
    {kind: "List", items: [{
      metadata: {name: $pod, namespace: "actual-budget"},
      spec: {hostUsers: false, containers: [{name: "actualbudget"}, {name: "enablebanking-seed"}]},
      status: {phase: "Running", containerStatuses: [
        {name: "actualbudget", ready: true}, {name: "enablebanking-seed", ready: true}]}}]}' \
    >"${fixtures}/pods.json"
  local container
  for container in actualbudget enablebanking-seed; do
    write_map "${container}.uid_map" "$(printf '         0 %s      65536\n' "${uid_base}")"
    write_map "${container}.gid_map" "$(printf '         0 %s      65536\n' "${gid_base}")"
  done
}

edit_pod() {
  jq "$1" "${fixtures}/pods.json" >"${fixtures}/p.json"
  mv "${fixtures}/p.json" "${fixtures}/pods.json"
}

rc=0
output=''
run_read() {
  rc=0
  output="$(FIXTURES="${fixtures}" EXPECTED_POD="${pod_name}" PATH="${fake_bin}:${PATH}" \
    bash "${script}" "$@" 2>&1)" || rc=$?
}

# Holds for every run that reached the cluster, whatever the verdict.
require_safe_run() {
  local marker
  for marker in UNEXPECTED_CONTEXT UNEXPECTED_VERB UNEXPECTED_EXEC; do
    [[ ! -e "${fixtures}/${marker}" ]] || fail "$1: the script tripped ${marker}"
  done
  refute_text "${uid_base}" "$1: a node-side user id reached the log"
  refute_text "${gid_base}" "$1: a node-side group id reached the log"
  refute_text "${pod_name}" "$1: the pod name reached the log"
  refute_text '198.51.100.9' "$1: an address reached the log"
  refute_text 'PRIVATE_HOST_USERS_CANARY' "$1: an unexpected field value reached the log"
}

cases_run=0
case_done() {
  cases_run=$((cases_run + 1))
  printf '  ok  %s\n' "$1"
}

printf 'test-read-actual-budget-userns-maps\n'

# --- usage -----------------------------------------------------------------
reset_fixtures
run_read
require_rc 1 'a missing --context must be a usage error'
[[ ! -e "${fixtures}/calls.log" ]] || fail 'a usage error must not reach the cluster'
run_read --context
require_rc 1 'an empty --context must be a usage error'
run_read --context admin@prod --unknown
require_rc 1 'an unknown flag must be a usage error'
[[ ! -e "${fixtures}/calls.log" ]] || fail 'a usage error must not reach the cluster'
case_done 'usage errors exit 1 before any read'

# --- the passing control ---------------------------------------------------
reset_fixtures
run_read --context admin@prod
require_rc 0 'four non-identity maps must be MAPPED'
require_safe_run 'control'
require_text 'MAPPED: both containers run in a user namespace' 'the control must print the MAPPED verdict'
require_text 'actualbudget uid_map: mapped' 'the control must report the server container user map'
require_text 'enablebanking-seed gid_map: mapped' 'the control must report the sidecar group map'
require_text '65536 ids in range' 'the control must report the range size'
[[ "$(wc -l <"${fixtures}/calls.log" | tr -d ' ')" -eq 5 ]] ||
  fail 'the control must issue exactly one get and four exec calls'
[[ "$(grep -c '^--context admin@prod get pods ' "${fixtures}/calls.log")" -eq 1 ]] ||
  fail 'the control must list pods exactly once'
[[ "$(grep -c '^--context admin@prod exec ' "${fixtures}/calls.log")" -eq 4 ]] ||
  fail 'the control must exec exactly four times'
if grep -Ev '^--context admin@prod (get|exec) ' "${fixtures}/calls.log" >/dev/null; then
  fail 'only get and exec may be issued'
fi
case_done 'four non-identity maps are MAPPED, from one get and four fixed exec calls'

reset_fixtures
write_map actualbudget.uid_map "$(printf '0 %s 1000\n1000 %s 64536\n' "${uid_base}" "$((uid_base + 1000))")"
run_read --context admin@prod
require_rc 0 'a map split over several ranges must still be MAPPED'
require_safe_run 'multi-range map'
require_text 'actualbudget uid_map: mapped — id 0 is a non-zero id on the node, 65536 ids in range.' \
  'the range sizes of a multi-line map must be summed'
case_done 'a multi-range non-identity map is MAPPED with its sizes summed'

reset_fixtures
edit_pod '.items[0].status.containerStatuses |= reverse'
run_read --context admin@prod
require_rc 0 'ready statuses in a different order must still be MAPPED'
require_safe_run 'reordered ready statuses'
require_text 'MAPPED: both containers run in a user namespace' 'reordered statuses must print MAPPED'
case_done 'ready statuses identify both expected containers regardless of order'

# --- IDENTITY: one fixture away from the control ---------------------------
identity_case() {
  reset_fixtures
  write_map "$1" "$2"
  run_read --context admin@prod
  require_rc 2 "$3 must be IDENTITY"
  require_safe_run "$3"
  require_text 'IDENTITY: at least one map exposes node identities' "$3 must print the IDENTITY verdict"
  refute_text 'MAPPED: both' "$3 must not print the MAPPED verdict"
  case_done "$3 is IDENTITY"
}
identity_case actualbudget.uid_map '         0          0 4294967295' 'the identity user map on the server'
identity_case enablebanking-seed.gid_map '         0          0 4294967295' 'the identity group map on the sidecar'
identity_case actualbudget.gid_map '0 0 65536' 'root mapped to the node root over a bounded range'
identity_case enablebanking-seed.uid_map "$(printf '0 %s 1000\n1000 1000 1\n' "${uid_base}")" \
  'one id passed straight through beside a mapped root'
identity_case actualbudget.uid_map "0 ${uid_base} 4294967295" 'a range covering the whole id space'
identity_case actualbudget.uid_map "$(printf '0 %s 1\n1 0 1\n' "${uid_base}")" \
  'a non-root container id mapped to the node root beside a mapped root'

# IDENTITY outranks an unreadable map: one proven identity map is the answer.
reset_fixtures
write_map actualbudget.uid_map '0 0 4294967295'
rm "${fixtures}/maps/enablebanking-seed.gid_map"
run_read --context admin@prod
require_rc 2 'an identity map beside an unreadable one must be IDENTITY'
require_safe_run 'identity beside unreadable'
require_text 'IDENTITY: at least one map exposes node identities' 'identity beside unreadable must print its verdict'
refute_text 'MAPPED: both' 'identity beside unreadable must not print the MAPPED verdict'
case_done 'an identity map beside an unreadable one is IDENTITY'

# --- INCONCLUSIVE: the maps ------------------------------------------------
map_inconclusive() {
  reset_fixtures
  if [[ "$2" == MISSING ]]; then
    rm "${fixtures}/maps/$1"
  else
    write_map "$1" "$2"
  fi
  run_read --context admin@prod
  require_rc 3 "$3 must be INCONCLUSIVE"
  require_safe_run "$3"
  require_text 'INCONCLUSIVE: at least one map could not be read or judged.' "$3 must say why"
  refute_text 'MAPPED: both' "$3 must not print the MAPPED verdict"
  case_done "$3 is INCONCLUSIVE"
}
map_inconclusive enablebanking-seed.uid_map MISSING 'a map that could not be read'
map_inconclusive actualbudget.gid_map '' 'an empty map'
map_inconclusive actualbudget.uid_map 'cat: /proc/self/uid_map: No such file' 'a map that is not numbers'
map_inconclusive actualbudget.uid_map "0 ${uid_base}" 'a map line with two fields'
map_inconclusive actualbudget.uid_map "0 ${uid_base} 65536 9" 'a map line with four fields'
map_inconclusive actualbudget.uid_map "0 ${uid_base} 0" 'a zero-length range'
map_inconclusive actualbudget.uid_map "1000 ${uid_base} 65536" 'a map that leaves id 0 unmapped'
map_inconclusive actualbudget.uid_map "0 99999999999 65536" 'an id longer than the kernel prints'
map_inconclusive actualbudget.uid_map "$(printf '0 %s 1000\n08 8 1\n' "${uid_base}")" \
  'an inside id with a leading zero beside a mapped root'
map_inconclusive enablebanking-seed.gid_map "$(printf '0 %s 1000\n8 08 1\n' "${gid_base}")" \
  'an outside id with a leading zero beside a mapped root'
map_inconclusive actualbudget.uid_map "$(printf '0 %s 65536\ngarbage\n' "${uid_base}")" \
  'a malformed line after a valid mapped range'

# --- INCONCLUSIVE: the pod -------------------------------------------------
pod_inconclusive() {
  run_read --context admin@prod
  require_rc 3 "$1 must be INCONCLUSIVE"
  require_safe_run "$1"
  require_text 'INCONCLUSIVE: ' "$1 must print the INCONCLUSIVE verdict"
  if grep -q ' exec ' "${fixtures}/calls.log"; then
    fail "$1: nothing may be exec'd when the pod precondition fails"
  fi
  case_done "$1 is INCONCLUSIVE with no exec"
}
reset_fixtures
rm "${fixtures}/pods.json"
pod_inconclusive 'a failed pod list'
reset_fixtures
printf 'not json' >"${fixtures}/pods.json"
pod_inconclusive 'a pod list that is not JSON'
reset_fixtures
edit_pod '.items = {unexpected: .items[0]}'
pod_inconclusive 'a pod list with an object instead of an items array'
reset_fixtures
edit_pod '.items[0].spec.containers = "unexpected"'
pod_inconclusive 'a container list that is not an array'
reset_fixtures
edit_pod '.items[0].status.containerStatuses = "unexpected"'
pod_inconclusive 'a container status list that is not an array'
reset_fixtures
edit_pod '.items[0].spec.containers |= {first: .[0], second: .[1]}'
pod_inconclusive 'an object with expected container values instead of an array'
reset_fixtures
edit_pod '.items[0].status.containerStatuses |= {first: .[0], second: .[1]}'
pod_inconclusive 'an object with expected status values instead of an array'
reset_fixtures
edit_pod '.items = []'
pod_inconclusive 'no pod'
reset_fixtures
edit_pod '.items += [.items[0]]'
pod_inconclusive 'two pods'
reset_fixtures
edit_pod '.items[0].status.phase = "Pending"'
pod_inconclusive 'a pod that is not Running'
reset_fixtures
edit_pod '.items[0].spec.hostUsers = true'
pod_inconclusive 'hostUsers true'
reset_fixtures
edit_pod 'del(.items[0].spec.hostUsers)'
pod_inconclusive 'hostUsers unset'
reset_fixtures
edit_pod '.items[0].spec.hostUsers = "PRIVATE_HOST_USERS_CANARY"'
pod_inconclusive 'an unexpected hostUsers value'
reset_fixtures
edit_pod '.items[0].spec.hostUsers = "false"'
pod_inconclusive 'a string instead of boolean hostUsers false'
reset_fixtures
edit_pod '.items[0].spec.containers += [{name: "extra"}]'
pod_inconclusive 'an unexpected third container'
reset_fixtures
edit_pod '.items[0].spec.containers = [{name: "actualbudget"}]'
pod_inconclusive 'a missing sidecar'
reset_fixtures
edit_pod '.items[0].status.containerStatuses[1].ready = false'
pod_inconclusive 'a container that is not ready'
reset_fixtures
edit_pod '.items[0].status.containerStatuses = []'
pod_inconclusive 'no container statuses'
reset_fixtures
edit_pod '.items[0].status.containerStatuses[1].name = "actualbudget"'
pod_inconclusive 'duplicate ready statuses that leave the sidecar unobserved'
reset_fixtures
edit_pod '.items[0].status.containerStatuses[1].name = "unrelated"'
pod_inconclusive 'an unrelated ready status instead of the sidecar'
reset_fixtures
edit_pod '.items[0].metadata.name = "--kubeconfig=/tmp/x"'
pod_inconclusive 'a pod name that is not an object name'

# --- workflow shape -----------------------------------------------------------
# The script is only as safe as the way it is dispatched, so the workflow's shape is pinned too.
readonly workflow="${root_dir}/.github/workflows/read-actual-budget-userns-maps.yaml"
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
confirm="$(wf_line "!= 'read-actual-budget-userns-maps'")"
restore="$(wf_line 'secrets.KUBE_CONFIG')"
endpoint="$(wf_line 'run: ./scripts/use-prod-stable-api-endpoint.sh >/dev/null')"
read_step="$(wf_line 'run: ./scripts/read-actual-budget-userns-maps.sh --context admin@prod')"
for step in "${main_guard}" "${checkout}" "${confirm}" "${restore}" "${endpoint}" "${read_step}"; do
  [[ -n "${step}" ]] || wf_fail 'a required step is missing'
done
[[ "${main_guard}" -lt "${checkout}" && "${checkout}" -lt "${confirm}" &&
  "${confirm}" -lt "${restore}" && "${restore}" -lt "${endpoint}" && "${endpoint}" -lt "${read_step}" ]] ||
  wf_fail 'steps must run as: main guard, checkout, confirmation, kubeconfig, endpoint, read'
readonly dollar='$'
grep -Fq "ref: ${dollar}{{ github.sha }}" "${workflow}" || wf_fail 'checkout must be pinned to the dispatch commit'
[[ "$(grep -c 'secrets\.' "${workflow}")" -eq 2 ]] ||
  wf_fail 'only KUBE_CONFIG and HCLOUD_TOKEN may be read, once each'
if grep -Fq "${dollar}{{ inputs." <(grep -v '^[[:space:]]*CONFIRM: ' "${workflow}"); then
  wf_fail 'the input may only reach bash through env'
fi
case_done 'the workflow is dispatch-only, main-only, confirmed and serialised with deploys'

printf '\nAll %d case(s) passed: read-actual-budget-userns-maps.sh behaviour is pinned.\n' "${cases_run}"
