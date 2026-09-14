#!/usr/bin/env bash
# The workflow-shape assertions match GitHub Actions and shell expressions that must appear verbatim,
# so single-quoted `${...}` literals are intended throughout.
# shellcheck disable=SC2016
# Pin the behaviour of scripts/probe-cilium-externalauth-crossnode.sh.
#
# WHY THIS EXISTS. The probe decides #2284 from real requests, and its mistakes are silent: a FIXED
# from a client that never crossed nodes, a FAULT-PERSISTS from a broken path or policy rather than a
# lost subrequest, or a verdict across replicas or nodes that moved mid-run. So each conclusive
# verdict has a control that differs in one fixture, and every INCONCLUSIVE path is exercised.
#
# It also pins what makes dispatching it acceptable: which verbs it issues (get, apply, logs and a
# run-scoped delete — never exec, patch or an unscoped delete), that cleanup runs whenever anything
# was created, that nothing is created over leftovers, that the pods are pinned to the right nodes,
# that no address, node name or redirect URL reaches the public log, and the workflow's shape.
#
# kubectl is faked from a fixture directory; no cluster, no secrets, no network. Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/probe-cilium-externalauth-crossnode.sh"
readonly workflow="${root_dir}/.github/workflows/probe-cilium-externalauth-crossnode.yaml"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

readonly fake_bin="${work_dir}/bin"
readonly fixtures="${work_dir}/fixtures"
readonly label='platform.devantler.tech/externalauth-probe'
readonly run_id='12345'
mkdir -p "${fake_bin}"

output=''
rc=''

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

calls() {
  cat "${fixtures}/calls.log"
}

# ---------------------------------------------------------------------------
# Fake kubectl. Records every invocation, serves fixtures, and flags anything the probe must never
# do. A failed call writes an address to stderr, the way a real connection error does, so the leak
# assertions also cover the error paths.
# ---------------------------------------------------------------------------
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/calls.log"
args=" $* "
if [[ "${args}" != " --context admin@prod "* ]]; then
  touch "${FIXTURES}/UNEXPECTED_CONTEXT"
  exit 1
fi
label='platform.devantler.tech/externalauth-probe'
serve() {
  if [[ -f "${FIXTURES}/$1" ]]; then
    cat "${FIXTURES}/$1"
  else
    printf 'error: dial tcp 198.51.100.9:6443: connect: connection refused\n' >&2
    exit 1
  fi
}
serve_per_call() {
  local base="$1" count=0
  [[ -f "${FIXTURES}/${base}-calls" ]] && count="$(cat "${FIXTURES}/${base}-calls")"
  count=$((count + 1))
  printf '%s' "${count}" >"${FIXTURES}/${base}-calls"
  if [[ "${count}" -ge 2 && -f "${FIXTURES}/${base}-after.json" ]]; then
    serve "${base}-after.json"
  else
    serve "${base}.json"
  fi
}
case "${args}" in
  *" exec "* | *" patch "* | *" edit "* | *" replace "* | *" scale "* | *" cordon "* | *" drain "*)
    touch "${FIXTURES}/FORBIDDEN_VERB"
    exit 1
    ;;
  *" -n oauth2-proxy get pods -l app.kubernetes.io/name=oauth2-proxy,app.kubernetes.io/instance=oauth2-proxy -o json ") serve_per_call oauth2-pods ;;
  *" get nodes -o json ") serve_per_call nodes ;;
  *" -n whoami get httproutes,ciliumnetworkpolicies,pods -l ${label} -o name ") serve leftovers.txt ;;
  *" -n oauth2-proxy get referencegrants -l ${label} -o name ") serve leftover-grants.txt ;;
  *" apply -f - ")
    cat >>"${FIXTURES}/applied.yaml"
    [[ -f "${FIXTURES}/apply-fails" ]] && exit 1
    exit 0
    ;;
  *" -n whoami get httproutes -l ${label}=12345 -o json ") serve routes.json ;;
  *" -n whoami get pod externalauth-probe-cross-12345 -o json ") serve pod-cross.json ;;
  *" -n whoami get pod externalauth-probe-same-12345 -o json ") serve pod-same.json ;;
  *" -n whoami logs externalauth-probe-cross-12345 ") serve log-cross.txt ;;
  *" -n whoami logs externalauth-probe-same-12345 ") serve log-same.txt ;;
  *" -n whoami delete pods,httproutes,ciliumnetworkpolicies -l ${label}=12345 --ignore-not-found --wait=false ")
    touch "${FIXTURES}/deleted-whoami"
    [[ -f "${FIXTURES}/delete-fails" ]] && exit 1
    exit 0
    ;;
  *" -n oauth2-proxy delete referencegrants -l ${label}=12345 --ignore-not-found --wait=false ")
    touch "${FIXTURES}/deleted-grants"
    exit 0
    ;;
  *)
    touch "${FIXTURES}/UNEXPECTED_CALL"
    exit 1
    ;;
esac
FAKE
chmod +x "${fake_bin}/kubectl"

readonly requests=4
readonly dex_redirect='https://dex.platform.example.test/auth?client_id=public-client'

# gen_log <file> <control-answer> <authz-answer> [<n-first-authz> <first-authz-answer>]
# Writes a complete client log: warm-up ok, <requests> rounds, done.
gen_log() {
  local file="$1" control="$2" authz="$3" first_n="${4:-0}" first="${5:-}" i
  {
    printf 'PROBE-WARMUP ok\n'
    for ((i = 1; i <= requests; i++)); do
      printf 'PROBE control %s\n' "${control}"
      if ((i <= first_n)); then
        printf 'PROBE authz %s\n' "${first}"
      else
        printf 'PROBE authz %s\n' "${authz}"
      fi
    done
    printf 'PROBE-DONE %s\n' "${requests}"
  } >"${file}"
}

# ---------------------------------------------------------------------------
# Base fixtures: the #2284 topology. Two oauth2-proxy replicas on prod-worker-1 and prod-worker-2;
# the control plane is tainted, so the cross-node client must land on prod-worker-3 and the
# same-node control on prod-worker-1 (first sorted replica node).
# ---------------------------------------------------------------------------
reset_fixtures() {
  rm -rf "${fixtures}"
  mkdir -p "${fixtures}"
  : >"${fixtures}/calls.log"
  : >"${fixtures}/leftovers.txt"
  : >"${fixtures}/leftover-grants.txt"

  cat >"${fixtures}/oauth2-pods.json" <<'JSON'
{"items":[
 {"metadata":{"name":"oauth2-proxy-7c9d-aaaaa","uid":"uid-pod-a"},"spec":{"nodeName":"prod-worker-1"},
  "status":{"phase":"Running","podIP":"10.244.22.235","conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"name":"oauth2-proxy-7c9d-bbbbb","uid":"uid-pod-b"},"spec":{"nodeName":"prod-worker-2"},
  "status":{"phase":"Running","podIP":"10.244.23.28","conditions":[{"type":"Ready","status":"True"}]}}
]}
JSON

  cat >"${fixtures}/nodes.json" <<'JSON'
{"items":[
 {"metadata":{"name":"prod-control-plane-1","uid":"uid-node-cp1"},
  "spec":{"taints":[{"key":"node-role.kubernetes.io/control-plane","effect":"NoSchedule"}]},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.2"}]}},
 {"metadata":{"name":"prod-worker-2","uid":"uid-node-w2"},"spec":{},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.4"}]}},
 {"metadata":{"name":"prod-worker-1","uid":"uid-node-w1"},"spec":{},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.3"}]}},
 {"metadata":{"name":"prod-worker-3","uid":"uid-node-w3"},"spec":{},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.5"}]}}
]}
JSON

  cat >"${fixtures}/routes.json" <<'JSON'
{"items":[
 {"metadata":{"name":"externalauth-probe-control-12345"},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}},
 {"metadata":{"name":"externalauth-probe-authz-12345"},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}}
]}
JSON

  printf '%s\n' '{"spec":{"nodeName":"prod-worker-3"},"status":{"phase":"Succeeded"}}' >"${fixtures}/pod-cross.json"
  printf '%s\n' '{"spec":{"nodeName":"prod-worker-1"},"status":{"phase":"Succeeded"}}' >"${fixtures}/pod-same.json"

  gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}"
  gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}"
}

run_probe() {
  set +e
  output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" PROBE_POLL_SECONDS=0 GITHUB_STEP_SUMMARY='' \
    bash "${script}" "$@" 2>&1)"
  rc=$?
  set -e
}

run_default() {
  run_probe --context admin@prod --run-id "${run_id}" --requests "${requests}" --timeout 1
}

# Invariants that hold for EVERY run that reached the cluster.
require_safe_surface() {
  [[ ! -e "${fixtures}/UNEXPECTED_CONTEXT" ]] || fail 'a call used another kube context'
  [[ ! -e "${fixtures}/FORBIDDEN_VERB" ]] || fail 'the probe issued a forbidden verb (exec, patch, edit, replace, scale, cordon or drain)'
  [[ ! -e "${fixtures}/UNEXPECTED_CALL" ]] || fail 'the probe issued a kubectl call outside its pinned surface'
  if calls | grep -E ' delete ' | grep -Fvq -- "-l ${label}=${run_id} "; then
    fail 'a delete was not scoped to this run'"'"'s label'
  fi
  refute_text 'prod-worker' 'a node name reached the log'
  refute_text 'prod-control-plane' 'a node name reached the log'
  refute_text '10.0.0.' 'a node address reached the log'
  refute_text '10.244.' 'a pod address reached the log'
  refute_text '198.51.100.9' 'a kubectl error address reached the log'
  refute_text 'dex.platform' 'a redirect URL reached the log'
  refute_text 'uid-' 'a UID reached the log'
}

require_cleaned_up() {
  [[ -e "${fixtures}/deleted-whoami" && -e "${fixtures}/deleted-grants" ]] ||
    fail 'objects were created but cleanup did not delete both namespaces'"'"' labelled objects'
  require_text 'CLEANUP: done' 'cleanup did not report completion'
}

require_nothing_created() {
  [[ ! -e "${fixtures}/applied.yaml" ]] || fail 'the probe created objects on a path that must refuse first'
  [[ ! -e "${fixtures}/deleted-whoami" ]] || fail 'the probe deleted objects it did not create'
}

cases=0
pass() {
  cases=$((cases + 1))
  printf 'ok %d - %s\n' "${cases}" "$1"
}

# ---------------------------------------------------------------------------
# Usage: refused before any kubectl call.
# ---------------------------------------------------------------------------
reset_fixtures
run_probe --run-id "${run_id}"
require_rc 1 'a missing --context must be a usage error'
[[ ! -s "${fixtures}/calls.log" ]] || fail 'a usage error still called kubectl'
pass 'missing --context is refused without touching the cluster'

reset_fixtures
run_probe --context admin@prod --run-id "${run_id}" --requests 08
require_rc 1 'a leading-zero request count must be a usage error'
refute_text 'value too great for base' 'the request count reached arithmetic as octal'
[[ ! -s "${fixtures}/calls.log" ]] || fail 'a usage error still called kubectl'
pass 'a leading-zero request count is rejected before arithmetic'

reset_fixtures
run_probe --context admin@prod --run-id "${run_id}" --timeout 09
require_rc 1 'a leading-zero timeout must be a usage error'
refute_text 'value too great for base' 'the timeout reached arithmetic as octal'
pass 'a leading-zero timeout is rejected before arithmetic'

reset_fixtures
run_probe --context admin@prod --run-id "${run_id}" --requests 100 --timeout 5
require_rc 1 'a budget over the cap must be a usage error'
require_text 'must not exceed 250 seconds' 'the budget refusal did not name the cap'
[[ ! -s "${fixtures}/calls.log" ]] || fail 'a usage error still called kubectl'
reset_fixtures
run_probe --context admin@prod --run-id "${run_id}" --requests 50 --timeout 5
require_rc 3 'the budget boundary itself (250) must be accepted (it then fails on fixture logs)'
pass 'the requests x timeout budget is capped at exactly 250 seconds'

reset_fixtures
run_probe --context admin@prod --run-id 0123
require_rc 1 'a run id with a leading zero must be a usage error'
reset_fixtures
run_probe --context admin@prod --run-id 'abc'
require_rc 1 'a non-numeric run id must be a usage error'
pass 'the run id is decimal digits only'

# ---------------------------------------------------------------------------
# FIXED, and what it created.
# ---------------------------------------------------------------------------
reset_fixtures
run_default
require_rc 0 'every request delivered must be conclusive'
require_text 'VERDICT: FIXED' 'all-delivered must read FIXED'
require_safe_surface
require_cleaned_up
applied="$(cat "${fixtures}/applied.yaml")"
grep -Fq 'name: externalauth-probe-cross-12345' <<<"${applied}" || fail 'the cross-node pod was not created'
cross_block="$(awk '/name: externalauth-probe-cross-12345/{f=1} f&&/kubernetes.io\/hostname:/{print; exit}' <<<"${applied}")"
[[ "${cross_block}" == *'kubernetes.io/hostname: prod-worker-3'* ]] ||
  fail "the cross-node pod must be pinned to the untainted node with no replica (got: ${cross_block})"
same_block="$(awk '/name: externalauth-probe-same-12345/{f=1} f&&/kubernetes.io\/hostname:/{print; exit}' <<<"${applied}")"
[[ "${same_block}" == *'kubernetes.io/hostname: prod-worker-1'* ]] ||
  fail "the same-node pod must be pinned to a replica node (got: ${same_block})"
[[ "$(grep -c 'type: ExternalAuth' <<<"${applied}")" -eq 1 ]] || fail 'exactly one route must carry the ExternalAuth filter'
grep -Fq -- '- control.externalauth-probe.invalid' <<<"${applied}" || fail 'the control route hostname is not the fixed .invalid name'
grep -Fq -- '- authz.externalauth-probe.invalid' <<<"${applied}" || fail 'the ExternalAuth route hostname is not the fixed .invalid name'
[[ "$(grep -c 'sectionName: http$' <<<"${applied}")" -eq 2 ]] || fail 'both probe routes must attach to the plain http listener'
[[ "$(grep -c "${label}: \"12345\"" <<<"${applied}")" -ge 6 ]] || fail 'every created object and the policy selector must carry the run label'
grep -Fq 'hostUsers: false' <<<"${applied}" || fail 'the probe pods must run in a user namespace (whoami requires it)'
grep -Fq 'automountServiceAccountToken: false' <<<"${applied}" || fail 'the probe pods must not mount a token'
# Top-level kinds only: the ReferenceGrant's `to:` entry legitimately names `kind: Service`.
created_kinds="$(grep -E '^kind: ' <<<"${applied}" | sort | uniq -c | awk '{print $1 "x" $3}' | paste -sd' ' -)"
[[ "${created_kinds}" == '1xCiliumNetworkPolicy 2xHTTPRoute 2xPod 1xReferenceGrant' ]] ||
  fail "the probe created a kind set outside its declared one (got: ${created_kinds})"
pass 'FIXED: all cross-node requests delivered; pods pinned, objects labelled, cleaned up'

# ---------------------------------------------------------------------------
# FAULT-PERSISTS and its one-fixture neighbours.
# ---------------------------------------------------------------------------
reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 2 '000 0 '
run_default
require_rc 0 'cross-node timeouts with a healthy control must be conclusive'
require_text 'VERDICT: FAULT-PERSISTS' 'cross-node timeouts must read FAULT-PERSISTS'
require_text '2 of 4 cross-node' 'the lost count must be reported'
require_safe_surface
require_cleaned_up
pass 'FAULT-PERSISTS: cross-node timeouts while the control route answers'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '403 0 '
run_default
require_text 'VERDICT: FAULT-PERSISTS' 'an EMPTY 403 is Envoy'"'"'s ext_authz error and counts as lost'
reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '503 19 '
run_default
require_text 'VERDICT: FAULT-PERSISTS' 'a 5xx on the ExternalAuth route counts as lost'
pass 'an empty 403 and a 5xx both count as lost'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '403 1480 '
run_default
require_rc 3 'a 403 WITH a body is not attributable'
require_text 'neither delivered nor lost' 'a bodied 403 must be classified as other'
require_cleaned_up
pass 'a 403 with a body is other, not lost'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '302 0 https://evil.example.test/login'
run_default
require_rc 3 'a redirect that is not the Dex login is not proof oauth2-proxy answered'
pass 'a 302 to anywhere but the Dex login is other, not delivered'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '401 21 '
run_default
require_text 'VERDICT: FIXED' 'a 401 is only ever produced by oauth2-proxy, so it counts as delivered'
pass 'a 401 counts as delivered'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 2 '000 0 '
gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '000 0 '
run_default
require_text 'VERDICT: FAULT-PERSISTS' 'same-node partial loss is expected (Envoy load-balances across both replicas)'
pass 'FAULT-PERSISTS tolerates partial same-node loss'

# ---------------------------------------------------------------------------
# INCONCLUSIVE paths: the verdict must not be taken.
# ---------------------------------------------------------------------------
reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 2 '000 0 '
gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}"
printf 'PROBE-WARMUP ok\nPROBE control 000 0 \nPROBE authz 000 0 \nPROBE control 200 45 \nPROBE authz 000 0 \nPROBE control 200 45 \nPROBE authz 302 0 %s\nPROBE control 200 45 \nPROBE authz 302 0 %s\nPROBE-DONE 4\n' \
  "${dex_redirect}" "${dex_redirect}" >"${fixtures}/log-cross.txt"
run_default
require_rc 3 'a control failure must block the FAULT-PERSISTS verdict'
require_text 'plain control route did not answer every time' 'the reason must name the control route'
require_cleaned_up
pass 'a single control-route failure makes cross-node loss unattributable'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '200 45 '
run_default
require_rc 3 'a 200 on the ExternalAuth route means the filter is not applied'
require_text 'neither delivered nor lost' 'an unfiltered 200 must be classified as other'
pass 'an unfiltered 200 is INCONCLUSIVE'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '000 0 '
gen_log "${fixtures}/log-same.txt" '200 45 ' '000 0 '
run_default
require_rc 3 'no delivery anywhere means the route or backend is broken, not #2284'
require_text 'even from a node with a local replica' 'the reason must name the missing same-node delivery'
pass 'no same-node delivery is INCONCLUSIVE'

reset_fixtures
gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '000 0 '
run_default
require_rc 3 'same-node loss with no cross-node loss is not the #2284 pattern'
pass 'same-node loss without cross-node loss is INCONCLUSIVE'

reset_fixtures
printf 'PROBE-WARMUP failed\n' >"${fixtures}/log-cross.txt"
run_default
require_rc 3 'a failed warm-up must be INCONCLUSIVE'
require_text 'during warm-up' 'the reason must name the warm-up'
require_cleaned_up
pass 'a failed warm-up is INCONCLUSIVE'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}"
sed '$d' "${fixtures}/log-cross.txt" >"${fixtures}/log-cross.trimmed"
grep -v 'PROBE authz' "${fixtures}/log-cross.trimmed" >"${fixtures}/log-cross.txt" || true
printf 'PROBE authz 302 0 %s\nPROBE authz 302 0 %s\nPROBE authz 302 0 %s\nPROBE-DONE 4\n' \
  "${dex_redirect}" "${dex_redirect}" "${dex_redirect}" >>"${fixtures}/log-cross.txt"
run_default
require_rc 3 'a log missing one answer must be INCONCLUSIVE'
require_text 'did not contain every expected answer' 'the reason must name the incomplete log'
pass 'an incomplete client log is INCONCLUSIVE'

reset_fixtures
sed 's/uid-pod-b/uid-pod-c/' "${fixtures}/oauth2-pods.json" >"${fixtures}/oauth2-pods-after.json"
run_default
require_rc 3 'a replica replaced during the run must be INCONCLUSIVE'
require_text 'replicas changed during the run' 'the reason must name the replica change'
require_cleaned_up
pass 'replicas changing mid-run is INCONCLUSIVE (and still cleans up)'

reset_fixtures
sed 's/uid-node-w3/uid-node-w9/' "${fixtures}/nodes.json" >"${fixtures}/nodes-after.json"
run_default
require_rc 3 'a node replaced during the run must be INCONCLUSIVE'
require_text 'node set changed during the run' 'the reason must name the node change'
pass 'the node set changing mid-run is INCONCLUSIVE'

reset_fixtures
printf 'pod/externalauth-probe-cross-999\n' >"${fixtures}/leftovers.txt"
run_default
require_rc 3 'leftovers from an earlier run must refuse the run'
require_text 'already exist' 'the reason must name the leftovers'
require_nothing_created
require_safe_surface
pass 'leftover probe objects refuse the run before anything is created or deleted'

reset_fixtures
printf 'referencegrant.gateway.networking.k8s.io/externalauth-probe-999\n' >"${fixtures}/leftover-grants.txt"
run_default
require_rc 3 'a leftover grant must refuse the run'
require_nothing_created
pass 'a leftover ReferenceGrant also refuses the run'

reset_fixtures
cat >"${fixtures}/oauth2-pods.json" <<'JSON'
{"items":[
 {"metadata":{"uid":"uid-pod-a"},"spec":{"nodeName":"prod-worker-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"uid":"uid-pod-b"},"spec":{"nodeName":"prod-worker-2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"uid":"uid-pod-c"},"spec":{"nodeName":"prod-worker-3"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}
]}
JSON
run_default
require_rc 3 'with a replica on every schedulable node no cross-node path can be forced'
require_text 'no schedulable node without an oauth2-proxy replica' 'the reason must name the missing cross node'
require_nothing_created
pass 'no replica-free schedulable node is INCONCLUSIVE before creating anything'

reset_fixtures
sed 's/"status":"True"}]}},$/"status":"False"}]}},/' "${fixtures}/oauth2-pods.json" >"${fixtures}/oauth2-pods.tmp"
mv "${fixtures}/oauth2-pods.tmp" "${fixtures}/oauth2-pods.json"
grep -Fq '"status":"False"' "${fixtures}/oauth2-pods.json" || fail 'fixture did not produce an unready replica'
run_default
require_rc 3 'an unready replica must be INCONCLUSIVE'
require_nothing_created
pass 'an unsettled replica is INCONCLUSIVE before creating anything'

reset_fixtures
touch "${fixtures}/apply-fails"
run_default
require_rc 3 'a failed apply must be INCONCLUSIVE'
require_cleaned_up
pass 'a failed apply still runs cleanup'

reset_fixtures
printf '%s\n' '{"items":[{"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"False"}]}]}},{"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}}]}' >"${fixtures}/routes.json"
run_default
require_rc 3 'a route with unresolved references must stop before the pods'
require_text 'not accepted with resolved references' 'the reason must name the route status'
if grep -Fq 'kind: Pod' "${fixtures}/applied.yaml"; then
  fail 'pods were created although the routes were not ready'
fi
require_cleaned_up
pass 'unresolved route references stop the run before any pod is created'

reset_fixtures
printf '%s\n' '{"spec":{"nodeName":"prod-worker-2"},"status":{"phase":"Succeeded"}}' >"${fixtures}/pod-cross.json"
run_default
require_rc 3 'a pod on another node must be INCONCLUSIVE'
require_text 'other than the one it was pinned to' 'the reason must name the placement'
pass 'a probe pod that ran elsewhere is INCONCLUSIVE'

reset_fixtures
printf '%s\n' '{"spec":{"nodeName":"prod-worker-3"},"status":{"phase":"Failed"}}' >"${fixtures}/pod-cross.json"
run_default
require_rc 3 'a failed pod must be INCONCLUSIVE'
require_cleaned_up
pass 'a failed probe pod is INCONCLUSIVE'

reset_fixtures
touch "${fixtures}/delete-fails"
run_default
require_rc 4 'a failed cleanup must override even a conclusive verdict'
require_text 'VERDICT: FIXED' 'the verdict itself should still be printed'
require_text 'CLEANUP: FAILED' 'the failed cleanup must be reported'
pass 'a failed cleanup exits 4 even after a conclusive verdict'

# ---------------------------------------------------------------------------
# Workflow shape. The literals below are GitHub Actions and shell expressions that must appear
# verbatim in the workflow, so single quotes are intended.
# ---------------------------------------------------------------------------
# shellcheck disable=SC2016
wf="$(cat "${workflow}")"
output="${wf}"
rc=0
on_block="$(awk '/^on:/{f=1; next} f&&/^[a-z]/{exit} f' "${workflow}")"
grep -Fq 'workflow_dispatch:' <<<"${on_block}" || fail 'the workflow must be dispatch-triggered'
if grep -Eq '^\s*(schedule|pull_request|pull_request_target|push|merge_group|workflow_run):' <<<"${on_block}"; then
  fail 'the workflow must have no trigger other than workflow_dispatch'
fi
guard_line="$(grep -n "refs/heads/main" "${workflow}" | head -1 | cut -d: -f1)"
checkout_line="$(grep -n 'uses: actions/checkout@' "${workflow}" | head -1 | cut -d: -f1)"
[[ -n "${guard_line}" && -n "${checkout_line}" && "${guard_line}" -lt "${checkout_line}" ]] ||
  fail 'the main-branch guard must run before checkout'
confirm_line="$(grep -n "probe-production-externalauth'" "${workflow}" | head -1 | cut -d: -f1)"
kubeconfig_line="$(grep -n 'KUBE_CONFIG: \${{ secrets.KUBE_CONFIG }}' "${workflow}" | head -1 | cut -d: -f1)"
[[ -n "${confirm_line}" && -n "${kubeconfig_line}" && "${confirm_line}" -lt "${kubeconfig_line}" ]] ||
  fail 'the confirmation phrase must be checked before the kubeconfig is restored'
require_text 'group: prod-deploy' 'the workflow must serialise on the prod-deploy lock'
require_text 'environment: prod' 'the workflow must use the prod environment'
require_text 'contents: read' 'the job token must be read-only'
require_text 'permissions: {}' 'the workflow must default to no permissions'
require_text '--context admin@prod --run-id "${RUN_ID}"' 'the workflow must pin the context and pass the run id'
require_text 'RUN_ID: ${{ github.run_id }}' 'the run id must come from github.run_id through env'
refute_text 'run: ./scripts/probe-cilium-externalauth-crossnode.sh --context admin@prod --run-id "${{' 'inputs must not be interpolated into run:'
job_minutes="$(sed -n 's/^    timeout-minutes: \([0-9][0-9]*\)$/\1/p' "${workflow}")"
[[ -n "${job_minutes}" ]] || fail 'the job must set timeout-minutes'
# Worst case the script allows: route wait (24 x 5s) + both pods waited to their deadline plus
# grace, where deadline = 2 * 250 + 2 * 30 + 60 and grace = 180, plus a few minutes of setup.
worst_seconds=$((24 * 5 + 2 * (2 * 250 + 2 * 30 + 60 + 180) + 180))
((job_minutes * 60 >= worst_seconds)) ||
  fail "timeout-minutes (${job_minutes}) is shorter than the script's worst case (${worst_seconds}s)"
grep -Fq 'readonly max_request_seconds=250' "${script}" || fail 'the budget cap the timeout was sized for changed'
pass 'the workflow is dispatch-only, guarded, serialised, least-privilege and sized for the worst case'

printf 'All %d probe cases passed.\n' "${cases}"
