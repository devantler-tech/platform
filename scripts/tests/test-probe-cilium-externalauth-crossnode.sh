#!/usr/bin/env bash
# The workflow-shape assertions match GitHub Actions and shell expressions that must appear verbatim,
# so single-quoted `${...}` literals are intended throughout.
# shellcheck disable=SC2016
# Pin the behaviour of scripts/probe-cilium-externalauth-crossnode.sh.
#
# WHY THIS EXISTS. The probe decides #2284 from real requests, and its mistakes are silent: a FIXED
# from a client that never crossed nodes, a FAULT-PERSISTS from a broken path, policy or a transient
# blip rather than the black-hole, or a verdict across endpoints or nodes that moved mid-run. So each
# conclusive verdict has a control that differs in one fixture, and every INCONCLUSIVE path is
# exercised.
#
# It also pins what makes dispatching it acceptable: which verbs it issues (get, apply, logs and a
# run-scoped delete — never exec, patch or an unscoped delete), that cleanup runs whenever anything
# was created, that nothing is created over leftovers, that the pods are pinned to the right nodes,
# that no address, node name or redirect URL reaches the public log, and the workflow's shape.
#
# Finally it RUNS the in-pod shell loop the script renders, against a fake curl, so escaping and the
# warm-up rules are exercised as the pod would execute them.
#
# kubectl and curl are faked; no cluster, no secrets, no network. Bash 3.2 compatible.
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
  *" -n oauth2-proxy get endpointslices -l kubernetes.io/service-name=oauth2-proxy -o json ") serve_per_call endpoints ;;
  *" -n kube-system get daemonsets cilium cilium-envoy -o json ") serve_per_call datapath ;;
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

readonly requests=10
readonly dex_redirect='https://dex.platform.example.test/auth?client_id=public-client'
readonly guard_ok='301 0 https://unrouted.externalauth-probe.invalid/'

# gen_log <file> <control-answer> <authz-answer> [<n-first-authz> <first-authz-answer> [<guard>]]
# Writes a complete client log: warm-up ok, <requests> rounds, the guard answer, done.
gen_log() {
  local file="$1" control="$2" authz="$3" first_n="${4:-0}" first="${5:-}" guard="${6:-${guard_ok}}" i
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
    printf 'PROBE guard %s\n' "${guard}"
    printf 'PROBE-DONE %s\n' "${requests}"
  } >"${file}"
}

# ---------------------------------------------------------------------------
# Base fixtures: the #2284 topology. Two oauth2-proxy endpoints on prod-worker-1 and prod-worker-2;
# the control plane is tainted, so the cross-node client must land on prod-worker-3 and the
# same-node control on prod-worker-1 (first sorted endpoint node). Hostname labels deliberately
# differ from node names, so pinning by name instead of label is caught.
# ---------------------------------------------------------------------------
reset_fixtures() {
  rm -rf "${fixtures}"
  mkdir -p "${fixtures}"
  : >"${fixtures}/calls.log"
  : >"${fixtures}/leftovers.txt"
  : >"${fixtures}/leftover-grants.txt"

  cat >"${fixtures}/endpoints.json" <<'JSON'
{"items":[{"endpoints":[
 {"addresses":["10.244.22.235"],"nodeName":"prod-worker-1","targetRef":{"kind":"Pod","uid":"uid-pod-a"},"conditions":{"ready":true,"serving":true,"terminating":false}},
 {"addresses":["10.244.23.28"],"nodeName":"prod-worker-2","targetRef":{"kind":"Pod","uid":"uid-pod-b"},"conditions":{"ready":true,"serving":true,"terminating":false}}
]}]}
JSON

  cat >"${fixtures}/nodes.json" <<'JSON'
{"items":[
 {"metadata":{"name":"prod-control-plane-1","uid":"uid-node-cp1","labels":{"kubernetes.io/hostname":"host-cp1"}},
  "spec":{"taints":[{"key":"node-role.kubernetes.io/control-plane","effect":"NoSchedule"}]},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.2"}]}},
 {"metadata":{"name":"prod-worker-2","uid":"uid-node-w2","labels":{"kubernetes.io/hostname":"host-w2"}},"spec":{},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.4"}]}},
 {"metadata":{"name":"prod-worker-1","uid":"uid-node-w1","labels":{"kubernetes.io/hostname":"host-w1"}},"spec":{},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.3"}]}},
 {"metadata":{"name":"prod-worker-3","uid":"uid-node-w3","labels":{"kubernetes.io/hostname":"host-w3"}},"spec":{},
  "status":{"conditions":[{"type":"Ready","status":"True"}],"addresses":[{"type":"InternalIP","address":"10.0.0.5"}]}}
]}
JSON

  cat >"${fixtures}/datapath.json" <<'JSON'
{"kind":"List","items":[
 {"metadata":{"name":"cilium","generation":7},"status":{"observedGeneration":7,"desiredNumberScheduled":4,"updatedNumberScheduled":4,"numberAvailable":4,"numberReady":4}},
 {"metadata":{"name":"cilium-envoy","generation":3},"status":{"observedGeneration":3,"desiredNumberScheduled":4,"updatedNumberScheduled":4,"numberAvailable":4,"numberReady":4}}
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
  output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" PROBE_POLL_SECONDS=0 PROBE_ROUTE_WAIT_SECONDS=1 GITHUB_STEP_SUMMARY='' \
    bash "${script}" "$@" 2>&1)"
  rc=$?
  set -e
}

run_default() {
  run_probe --context admin@prod --run-id "${run_id}" --requests "${requests}" --timeout 1
}

# Invariants that hold for EVERY run.
require_safe_surface() {
  [[ ! -e "${fixtures}/UNEXPECTED_CONTEXT" ]] || fail 'a call used another kube context'
  [[ ! -e "${fixtures}/FORBIDDEN_VERB" ]] || fail 'the probe issued a forbidden verb (exec, patch, edit, replace, scale, cordon or drain)'
  [[ ! -e "${fixtures}/UNEXPECTED_CALL" ]] || fail 'the probe issued a kubectl call outside its pinned surface'
  if grep -Fvq -- '--context admin@prod --request-timeout=30s ' "${fixtures}/calls.log"; then
    fail 'a kubectl call was not bounded by --request-timeout'
  fi
  if calls | grep -E ' delete ' | grep -Fvq -- "-l ${label}=${run_id} "; then
    fail 'a delete was not scoped to this run'"'"'s label'
  fi
  refute_text 'prod-worker' 'a node name reached the log'
  refute_text 'prod-control-plane' 'a node name reached the log'
  refute_text 'host-w' 'a hostname label reached the log'
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

require_inconclusive() {
  require_rc 3 "$1"
  require_text 'VERDICT: INCONCLUSIVE' "$1 (verdict)"
  require_text "$2" "$1 (reason)"
  require_safe_surface
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
[[ ! -s "${fixtures}/calls.log" ]] || fail 'a usage error still called kubectl'
pass 'a leading-zero timeout is rejected before arithmetic'

reset_fixtures
run_probe --context admin@prod --run-id "${run_id}" --requests 100 --timeout 5
require_rc 1 'a budget over the cap must be a usage error'
require_text 'must not exceed 250 seconds' 'the budget refusal did not name the cap'
[[ ! -s "${fixtures}/calls.log" ]] || fail 'a usage error still called kubectl'
reset_fixtures
run_probe --context admin@prod --run-id "${run_id}" --requests 50 --timeout 5
require_inconclusive 'the budget boundary itself (250) must be accepted and reach the cluster' 'did not contain every expected answer'
pass 'the requests x timeout budget is capped at exactly 250 seconds'

reset_fixtures
run_probe --context admin@prod --run-id 0123
require_rc 1 'a run id with a leading zero must be a usage error'
reset_fixtures
run_probe --context admin@prod --run-id 'abc'
require_rc 1 'a non-numeric run id must be a usage error'
[[ ! -s "${fixtures}/calls.log" ]] || fail 'a usage error still called kubectl'
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
[[ "${cross_block}" == *'kubernetes.io/hostname: host-w3'* ]] ||
  fail "the cross-node pod must be pinned by the hostname label of the untainted node with no endpoint (got: ${cross_block})"
same_block="$(awk '/name: externalauth-probe-same-12345/{f=1} f&&/kubernetes.io\/hostname:/{print; exit}' <<<"${applied}")"
[[ "${same_block}" == *'kubernetes.io/hostname: host-w1'* ]] ||
  fail "the same-node pod must be pinned by the hostname label of an endpoint node (got: ${same_block})"
[[ "$(grep -c 'type: ExternalAuth' <<<"${applied}")" -eq 1 ]] || fail 'exactly one route must carry the ExternalAuth filter'
grep -Fq -- '- control.externalauth-probe.invalid' <<<"${applied}" || fail 'the control route hostname is not the fixed .invalid name'
grep -Fq -- '- authz.externalauth-probe.invalid' <<<"${applied}" || fail 'the ExternalAuth route hostname is not the fixed .invalid name'
if grep -Fq -- '- unrouted.externalauth-probe.invalid' <<<"${applied}"; then
  fail 'the guard hostname must stay unrouted'
fi
[[ "$(grep -c 'sectionName: http$' <<<"${applied}")" -eq 2 ]] || fail 'both probe routes must attach to the plain http listener'
[[ "$(grep -c "${label}: \"12345\"" <<<"${applied}")" -ge 6 ]] || fail 'every created object and the policy selector must carry the run label'
grep -Fq 'hostUsers: false' <<<"${applied}" || fail 'the probe pods must run in a user namespace (whoami requires it)'
grep -Fq 'automountServiceAccountToken: false' <<<"${applied}" || fail 'the probe pods must not mount a token'
# Top-level kinds only: the ReferenceGrant's `to:` entry legitimately names `kind: Service`.
created_kinds="$(grep -E '^kind: ' <<<"${applied}" | sort | uniq -c | awk '{print $1 "x" $3}' | paste -sd' ' -)"
[[ "${created_kinds}" == '1xCiliumNetworkPolicy 2xHTTPRoute 2xPod 1xReferenceGrant' ]] ||
  fail "the probe created a kind set outside its declared one (got: ${created_kinds})"
pass 'FIXED: all cross-node requests delivered; pods pinned by label, objects labelled, cleaned up'

# ---------------------------------------------------------------------------
# FAULT-PERSISTS and its one-fixture neighbours.
# ---------------------------------------------------------------------------
reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '000 0 '
gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}" 5 '000 0 '
run_default
require_rc 0 'a complete cross-node black-hole with a healthy control must be conclusive'
require_text 'VERDICT: FAULT-PERSISTS' 'a complete cross-node black-hole must read FAULT-PERSISTS'
require_text '10 of 10 cross-node' 'the lost count must be reported'
require_safe_surface
require_cleaned_up
pass 'FAULT-PERSISTS: a complete cross-node black-hole while the control route answers'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 9 '000 0 '
run_default
require_text 'VERDICT: FAULT-PERSISTS' '9 of 10 lost meets the 90% bar'
reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 8 '000 0 '
run_default
require_inconclusive '8 of 10 lost is below the 90% bar' 'intermittent loss'
pass 'FAULT-PERSISTS needs at least 90% cross-node loss (9 of 10 yes, 8 of 10 no)'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '000 0 '
run_default
require_inconclusive 'one transient timeout must not be conclusive either way' 'intermittent loss'
pass 'a single lost request is INCONCLUSIVE, not FAULT-PERSISTS'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '000 0 '
gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}" 10 '000 0 '
run_default
require_inconclusive 'no delivery anywhere means the route or backend is broken, not #2284' 'even from a node with a local endpoint'
# Cross-node 9 of 10 lost meets the 90% bar on its own, but the same-node client lost just as many
# (9, with 1 delivered), so the loss is not specific to the cross-node path.
reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 9 '000 0 '
gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}" 9 '000 0 '
run_default
require_inconclusive 'cross-node loss not above same-node loss is not the #2284 pattern' 'intermittent loss'
pass 'FAULT-PERSISTS needs same-node delivery and cross-node loss above same-node loss'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '403 0 '
run_default
require_text 'VERDICT: FAULT-PERSISTS' 'an EMPTY 403 is Envoy'"'"'s ext_authz error and counts as lost'
for code in 502 503 504; do
  reset_fixtures
  gen_log "${fixtures}/log-cross.txt" '200 45 ' "${code} 19 "
  run_default
  require_text 'VERDICT: FAULT-PERSISTS' "a ${code} on the ExternalAuth route counts as lost"
done
pass 'an empty 403 and 502/503/504 count as lost'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '500 12 '
run_default
require_inconclusive 'a 500 came from a backend that answered, so it is not a lost subrequest' 'status codes: cross 500, same -'
pass 'a 500 is other, not lost, and its status code is printed'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '403 1480 '
run_default
require_inconclusive 'a 403 WITH a body is not attributable' 'status codes: cross 403'
pass 'a 403 with a body is other, not lost'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '302 0 https://evil.example.test/login'
run_default
require_inconclusive 'a redirect that is not the Dex login is not proof oauth2-proxy answered' 'status codes: cross 302'
refute_text 'evil.example.test' 'a redirect URL reached the log'
pass 'a 302 to anywhere but the Dex login is other, not delivered'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '401 21 '
run_default
require_text 'VERDICT: FIXED' 'a 401 is only ever produced by oauth2-proxy, so it counts as delivered'
pass 'a 401 counts as delivered'

# ---------------------------------------------------------------------------
# INCONCLUSIVE paths: the verdict must not be taken.
# ---------------------------------------------------------------------------
reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '000 0 '
# A complete cross-node black-hole (the FAULT-PERSISTS fixture) with exactly one control failure.
awk '/^PROBE control 200 45 $/ && !done {print "PROBE control 000 0 "; done = 1; next} {print}' \
  "${fixtures}/log-cross.txt" >"${fixtures}/log-cross.tmp"
mv "${fixtures}/log-cross.tmp" "${fixtures}/log-cross.txt"
[[ "$(grep -c '^PROBE control 000 0 $' "${fixtures}/log-cross.txt")" -eq 1 ]] || fail 'fixture did not produce exactly one control failure'
run_default
require_inconclusive 'a control failure must block the FAULT-PERSISTS verdict' 'plain control route did not answer every time'
require_cleaned_up
pass 'a single control-route failure makes cross-node loss unattributable'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' "302 0 ${dex_redirect}" 0 '' '302 0 https://dex.platform.example.test/auth'
run_default
require_inconclusive 'the redirect route answering anything but 301 means the probe affected other traffic' 'instead of 301'
pass 'the unrouted-hostname guard must still get the listener redirect'

reset_fixtures
gen_log "${fixtures}/log-cross.txt" '200 45 ' '200 45 '
run_default
require_inconclusive 'a 200 on the ExternalAuth route means the filter is not applied' 'status codes: cross 200'
pass 'an unfiltered 200 is INCONCLUSIVE'

reset_fixtures
gen_log "${fixtures}/log-same.txt" '200 45 ' "302 0 ${dex_redirect}" 1 '000 0 '
run_default
require_inconclusive 'same-node loss with no cross-node loss is not the #2284 pattern' 'not the #2284 pattern'
pass 'same-node loss without cross-node loss is INCONCLUSIVE'

reset_fixtures
# Logs that match the requested count, all delivered: only the minimum-sample rule can stop FIXED.
{
  printf 'PROBE-WARMUP ok\n'
  for ((i = 1; i <= 9; i++)); do printf 'PROBE control 200 45 \nPROBE authz 302 0 %s\n' "${dex_redirect}"; done
  printf 'PROBE guard %s\nPROBE-DONE 9\n' "${guard_ok}"
} >"${fixtures}/log-cross.txt"
cp "${fixtures}/log-cross.txt" "${fixtures}/log-same.txt"
run_probe --context admin@prod --run-id "${run_id}" --requests 9 --timeout 1
require_inconclusive 'a complete 9-request run is a smoke run' 'smoke run and never conclusive'
pass 'fewer than 10 requests per client is never conclusive'

reset_fixtures
printf 'PROBE-WARMUP failed\n' >"${fixtures}/log-cross.txt"
run_default
require_inconclusive 'a failed warm-up must be INCONCLUSIVE' 'during warm-up'
require_cleaned_up
pass 'a failed warm-up is INCONCLUSIVE'

reset_fixtures
awk 'BEGIN{d=0} /^PROBE authz / && !d {d=1; next} {print}' "${fixtures}/log-cross.txt" >"${fixtures}/log-cross.tmp"
mv "${fixtures}/log-cross.tmp" "${fixtures}/log-cross.txt"
[[ "$(grep -c '^PROBE authz ' "${fixtures}/log-cross.txt")" -eq 9 ]] || fail 'fixture did not drop exactly one answer'
run_default
require_inconclusive 'a log missing one answer must be INCONCLUSIVE' 'did not contain every expected answer'
reset_fixtures
grep -v '^PROBE guard ' "${fixtures}/log-same.txt" >"${fixtures}/log-same.tmp"
mv "${fixtures}/log-same.tmp" "${fixtures}/log-same.txt"
run_default
require_inconclusive 'a log missing the guard answer must be INCONCLUSIVE' 'did not contain every expected answer'
pass 'an incomplete client log (missing an answer or the guard) is INCONCLUSIVE'

reset_fixtures
sed 's/uid-pod-b/uid-pod-c/' "${fixtures}/endpoints.json" >"${fixtures}/endpoints-after.json"
run_default
require_inconclusive 'an endpoint replaced during the run must be INCONCLUSIVE' 'endpoints changed during the run'
require_cleaned_up
pass 'endpoints changing mid-run is INCONCLUSIVE (and still cleans up)'

reset_fixtures
sed 's/"10.244.23.28"/"10.244.23.99"/' "${fixtures}/endpoints.json" >"${fixtures}/endpoints-after.json"
grep -Fq '"10.244.23.99"' "${fixtures}/endpoints-after.json" || fail 'fixture did not change the endpoint address'
run_default
require_inconclusive 'an endpoint address changing with the same UID and node must be INCONCLUSIVE' 'endpoints changed during the run'
pass 'an endpoint address change mid-run is INCONCLUSIVE'

reset_fixtures
sed 's/"updatedNumberScheduled":4,"numberAvailable":4,"numberReady":4}},$/"updatedNumberScheduled":3,"numberAvailable":4,"numberReady":4}},/' "${fixtures}/datapath.json" >"${fixtures}/datapath.tmp"
mv "${fixtures}/datapath.tmp" "${fixtures}/datapath.json"
grep -Fq '"updatedNumberScheduled":3' "${fixtures}/datapath.json" || fail 'fixture did not produce a partial rollout'
run_default
require_inconclusive 'a partially rolled Cilium DaemonSet must be INCONCLUSIVE' 'rollout is incomplete'
require_nothing_created
reset_fixtures
printf '%s\n' '{"kind":"List","items":[{"metadata":{"name":"cilium","generation":7},"status":{"observedGeneration":7,"desiredNumberScheduled":4,"updatedNumberScheduled":4,"numberAvailable":4,"numberReady":4}}]}' >"${fixtures}/datapath.json"
run_default
require_inconclusive 'a missing cilium-envoy DaemonSet must be INCONCLUSIVE' 'was not found'
require_nothing_created
pass 'an incomplete or missing datapath rollout is INCONCLUSIVE before creating anything'

reset_fixtures
sed 's/"generation":3},"status":{"observedGeneration":3/"generation":4},"status":{"observedGeneration":4/' "${fixtures}/datapath.json" >"${fixtures}/datapath-after.json"
grep -Fq '"generation":4}' "${fixtures}/datapath-after.json" || fail 'fixture did not change the cilium-envoy generation'
run_default
require_inconclusive 'a datapath rollout during the run must be INCONCLUSIVE' 'rollout changed during the run'
require_cleaned_up
pass 'a Cilium or cilium-envoy rollout during the run is INCONCLUSIVE (and still cleans up)'

reset_fixtures
sed 's/uid-node-w3/uid-node-w9/' "${fixtures}/nodes.json" >"${fixtures}/nodes-after.json"
run_default
require_inconclusive 'a node replaced during the run must be INCONCLUSIVE' 'node set changed during the run'
pass 'the node set changing mid-run is INCONCLUSIVE'

reset_fixtures
printf 'pod/externalauth-probe-cross-999\n' >"${fixtures}/leftovers.txt"
run_default
require_inconclusive 'leftovers from an earlier run must refuse the run' 'already exist'
require_nothing_created
pass 'leftover probe objects refuse the run before anything is created or deleted'

reset_fixtures
printf 'referencegrant.gateway.networking.k8s.io/externalauth-probe-999\n' >"${fixtures}/leftover-grants.txt"
run_default
require_inconclusive 'a leftover grant must refuse the run' 'already exist'
require_nothing_created
pass 'a leftover ReferenceGrant also refuses the run'

reset_fixtures
cat >"${fixtures}/endpoints.json" <<'JSON'
{"items":[{"endpoints":[
 {"addresses":["10.244.22.235"],"nodeName":"prod-worker-1","targetRef":{"uid":"uid-pod-a"},"conditions":{"ready":true,"terminating":false}},
 {"addresses":["10.244.23.28"],"nodeName":"prod-worker-2","targetRef":{"uid":"uid-pod-b"},"conditions":{"ready":true,"terminating":false}},
 {"addresses":["10.244.24.40"],"nodeName":"prod-worker-3","targetRef":{"uid":"uid-pod-c"},"conditions":{"ready":true,"terminating":false}}
]}]}
JSON
run_default
require_inconclusive 'with an endpoint on every schedulable node no cross-node path can be forced' 'no schedulable node without an oauth2-proxy endpoint'
require_nothing_created
pass 'no endpoint-free schedulable node is INCONCLUSIVE before creating anything'

reset_fixtures
sed 's|"kubernetes.io/hostname":"host-w3"|"kubernetes.io/hostname":""|' "${fixtures}/nodes.json" >"${fixtures}/nodes.tmp"
mv "${fixtures}/nodes.tmp" "${fixtures}/nodes.json"
grep -Fq '"kubernetes.io/hostname":""' "${fixtures}/nodes.json" || fail 'fixture did not blank the hostname label'
run_default
require_inconclusive 'a candidate node without a hostname label cannot be pinned' 'no schedulable node without an oauth2-proxy endpoint'
require_nothing_created
pass 'a node with no hostname label is never chosen'

reset_fixtures
sed 's/"ready":true,"serving":true,"terminating":false}},$/"ready":false,"serving":true,"terminating":false}},/' "${fixtures}/endpoints.json" >"${fixtures}/endpoints.tmp"
mv "${fixtures}/endpoints.tmp" "${fixtures}/endpoints.json"
grep -Fq '"ready":false' "${fixtures}/endpoints.json" || fail 'fixture did not produce an unready endpoint'
run_default
require_inconclusive 'an unready endpoint must be INCONCLUSIVE' 'not settled'
require_nothing_created
reset_fixtures
printf '%s\n' '{"items":[{"endpoints":[]}]}' >"${fixtures}/endpoints.json"
run_default
require_inconclusive 'no endpoints at all must be INCONCLUSIVE' 'has no endpoints'
require_nothing_created
pass 'unsettled or missing endpoints are INCONCLUSIVE before creating anything'

reset_fixtures
touch "${fixtures}/apply-fails"
run_default
require_inconclusive 'a failed apply must be INCONCLUSIVE' 'could not create the probe routes'
require_cleaned_up
pass 'a failed apply still runs cleanup'

reset_fixtures
printf '%s\n' '{"items":[{"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"False"}]}]}},{"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}}]}' >"${fixtures}/routes.json"
run_default
require_inconclusive 'a route with unresolved references must stop before the pods' 'not accepted with resolved references'
if grep -Fq 'kind: Pod' "${fixtures}/applied.yaml"; then
  fail 'pods were created although the routes were not ready'
fi
require_cleaned_up
pass 'unresolved route references stop the run before any pod is created'

reset_fixtures
printf '%s\n' '{"spec":{"nodeName":"prod-worker-2"},"status":{"phase":"Succeeded"}}' >"${fixtures}/pod-cross.json"
run_default
require_inconclusive 'a pod on another node must be INCONCLUSIVE' 'other than the one it was pinned to'
pass 'a probe pod that ran elsewhere is INCONCLUSIVE'

reset_fixtures
printf '%s\n' '{"spec":{"nodeName":"prod-worker-1"},"status":{"phase":"Failed"}}' >"${fixtures}/pod-same.json"
run_default
require_inconclusive 'a failed pod must be INCONCLUSIVE' 'did not complete'
require_cleaned_up
pass 'a failed probe pod is INCONCLUSIVE'

reset_fixtures
touch "${fixtures}/delete-fails"
run_default
require_rc 4 'a failed cleanup must override even a conclusive verdict'
require_text 'VERDICT: FIXED' 'the verdict itself should still be printed'
require_text 'CLEANUP: FAILED' 'the failed cleanup must be reported'
[[ -e "${fixtures}/deleted-grants" ]] || fail 'a failed whoami delete must not skip the ReferenceGrant delete'
require_safe_surface
pass 'a failed cleanup exits 4 even after a conclusive verdict, and still attempts every delete'

# ---------------------------------------------------------------------------
# The in-pod loop, executed as rendered, against a fake curl.
# ---------------------------------------------------------------------------
reset_fixtures
run_default
readonly pod_loop="${work_dir}/pod-loop.sh"
awk '
  /name: externalauth-probe-cross-12345/ { pod = 1 }
  pod && /^        - \|$/ { body = 1; next }
  body && /^      securityContext:/ { exit }
  body { sub(/^          /, ""); print }
' "${fixtures}/applied.yaml" >"${pod_loop}"
grep -Fq 'PROBE-DONE' "${pod_loop}" || fail 'could not extract the rendered pod loop'
if grep -Fq '\$' "${pod_loop}"; then
  fail 'the rendered pod loop still contains an escaped \$'
fi

readonly fake_curl_bin="${work_dir}/curl-bin"
mkdir -p "${fake_curl_bin}"
cat >"${fake_curl_bin}/curl" <<'CURL'
#!/usr/bin/env bash
# Answers by Host header, per CURL_MODE:
#   delivered  control 200, authz 302 to Dex, unrouted 301
#   blackhole  control 200, authz times out (000), unrouted 301
#   unrouted   control 200, authz 404 (route not programmed yet), unrouted 301
host=''
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == '-H' ]]; then host="${2#Host: }"; shift 2; else shift; fi
done
case "${host}" in
  control.*) printf '200 45 ' ;;
  unrouted.*) printf '301 0 https://%s/' "${host}" ;;
  authz.*)
    case "${CURL_MODE}" in
      delivered) printf '302 0 https://dex.platform.example.test/auth' ;;
      blackhole) printf '000 0 '; exit 28 ;;
      unrouted) printf '404 0 ' ;;
    esac
    ;;
esac
CURL
chmod +x "${fake_curl_bin}/curl"

run_loop() {
  set +e
  output="$(PATH="${fake_curl_bin}:${PATH}" CURL_MODE="$1" REQUESTS=3 TIMEOUT=1 WARMUP_ATTEMPTS=2 \
    CONTROL_HOST=control.externalauth-probe.invalid AUTHZ_HOST=authz.externalauth-probe.invalid \
    GUARD_HOST=unrouted.externalauth-probe.invalid GATEWAY_URL=http://gateway.invalid/ \
    sh "${pod_loop}" 2>&1)"
  rc=$?
  set -e
}

run_loop delivered
require_rc 0 'the rendered loop must exit 0'
require_text 'PROBE-WARMUP ok' 'a delivering route must warm up'
[[ "$(grep -c '^PROBE authz 302 0 https://dex\.' <<<"${output}")" -eq 3 ]] || fail 'the loop must print one delivered answer per round'
require_text 'PROBE guard 301 0 https://unrouted.externalauth-probe.invalid/' 'the loop must print the guard answer'
require_text 'PROBE-DONE 3' 'the loop must report its round count'
run_loop blackhole
require_text 'PROBE-WARMUP ok' 'a COMPLETE black-hole must still warm up, so it is measured rather than reported as a broken path'
[[ "$(grep -c '^PROBE authz 000 0 $' <<<"${output}")" -eq 3 ]] || fail 'every black-holed request must print 000'
run_loop unrouted
require_text 'PROBE-WARMUP failed' 'an unprogrammed ExternalAuth route (404) must fail the warm-up'
refute_text 'PROBE authz' 'nothing may be counted before the routes are programmed'
pass 'the rendered in-pod loop runs as a pod would: delivered, complete black-hole, unprogrammed route'

# ---------------------------------------------------------------------------
# Workflow shape.
# ---------------------------------------------------------------------------
wf="$(cat "${workflow}")"
output="${wf}"
rc=0
on_block="$(awk '/^on:/{f=1; next} f&&/^[a-z]/{exit} f' "${workflow}")"
grep -Fq 'workflow_dispatch:' <<<"${on_block}" || fail 'the workflow must be dispatch-triggered'
if grep -Eq '^[[:space:]]*(schedule|pull_request|pull_request_target|push|merge_group|workflow_run):' <<<"${on_block}"; then
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
# Worst case the script allows (timeout 10, requests x timeout = 250, warm-up 12):
#   route wait (120s wall clock) + one in-flight 30s request past its deadline
#   + the shared pod wait (deadline + 180s grace) + one in-flight request
#   + ~24 other bounded kubectl calls (topology, datapath, leftovers, applies, logs, cleanup) at 30s
#   + 10 minutes of job setup and slack.
# The deadline formula is the script's own.
max_deadline=$(((2 * 25 + 2 * 12 + 1) * 10 + 12 + 60))
worst_seconds=$((120 + 35 + max_deadline + 180 + 35 + 24 * 30 + 600))
grep -Fq 'readonly route_wait_seconds="${PROBE_ROUTE_WAIT_SECONDS:-120}"' "${script}" || fail 'the route wait bound the timeout was sized for changed'
grep -Fq "readonly request_timeout='30s'" "${script}" || fail 'the request timeout the job timeout was sized for changed'
((job_minutes * 60 >= worst_seconds)) ||
  fail "timeout-minutes (${job_minutes}) is shorter than the script's worst case (${worst_seconds}s)"
grep -Fq 'readonly max_request_seconds=250' "${script}" || fail 'the budget cap the timeout was sized for changed'
grep -Fq 'readonly warmup_attempts=12' "${script}" || fail 'the warm-up bound the timeout was sized for changed'
grep -Fq 'readonly pod_deadline_seconds=$(((2 * requests + 2 * warmup_attempts + 1) * timeout + warmup_attempts + 60))' "${script}" ||
  fail 'the pod deadline formula the timeout was sized for changed'
pass 'the workflow is dispatch-only, guarded, serialised, least-privilege and sized for the worst case'

printf 'All %d probe cases passed.\n' "${cases}"
