#!/usr/bin/env bash
# The workflow-shape assertions match GitHub Actions and shell expressions that must appear verbatim,
# so single-quoted `${...}` literals are intended throughout.
# shellcheck disable=SC2016
# Pin the behaviour of scripts/probe-cilium-externalauth-crossnode.sh.
#
# WHY THIS EXISTS. The probe decides #2284 from real requests, and its mistakes are silent: a FIXED
# from pairs that never crossed nodes, a FAULT-PERSISTS from a broken path, policy or a transient
# blip rather than the black-hole, or a verdict across endpoints or nodes that moved mid-run. So each
# conclusive verdict has a control that differs in one fixture, every INCONCLUSIVE path is
# exercised, and two deliberately broken copies of the classifier must fail the verdict fixture.
#
# It also pins what makes dispatching it acceptable: which kubectl verbs it issues (get, apply and a
# run-scoped delete — never exec, patch or an unscoped delete), that it creates only two routes and
# a grant, that cleanup runs whenever anything was created, that nothing is created over leftovers,
# how every request is sent (through the load balancer's public address, certificate checks skipped,
# no PROXY header of its own, bounded by a timeout, the two routes of a pair in ONE curl invocation),
# that no address, node name or redirect URL reaches the public log, that the load balancer address
# is masked, and the workflow's shape.
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
# The load balancer's public address (a documentation range), and the URL every request must use.
readonly lb_address='203.0.113.10'
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
  *" exec "* | *" patch "* | *" edit "* | *" replace "* | *" scale "* | *" cordon "* | *" drain "* | *" logs "*)
    touch "${FIXTURES}/FORBIDDEN_VERB"
    exit 1
    ;;
  *" -n oauth2-proxy get endpointslices -l kubernetes.io/service-name=oauth2-proxy -o json ") serve_per_call endpoints ;;
  *" -n whoami get endpointslices -l kubernetes.io/service-name=whoami -o json ") serve_per_call backend ;;
  *" -n kube-system get daemonsets cilium cilium-envoy -o json ") serve_per_call datapath ;;
  *" -n kube-system get service cilium-gateway-platform -o json ") serve gateway.json ;;
  *" get ciliumnodes -o json ") serve_per_call ciliumnodes ;;
  *" get nodes -o json ") serve_per_call nodes ;;
  *" -n whoami get httproutes,ciliumnetworkpolicies,pods -l ${label} -o name ") serve leftovers.txt ;;
  *" -n oauth2-proxy get referencegrants -l ${label} -o name ") serve leftover-grants.txt ;;
  *" apply -f - ")
    cat >>"${FIXTURES}/applied.yaml"
    [[ -f "${FIXTURES}/apply-fails" ]] && exit 1
    exit 0
    ;;
  *" -n whoami get httproutes -l ${label}=12345 -o json ") serve routes.json ;;
  *" -n whoami delete httproutes -l ${label}=12345 --ignore-not-found --wait=true --timeout=25s ")
    touch "${FIXTURES}/deleted-whoami"
    [[ -f "${FIXTURES}/delete-fails" ]] && exit 1
    exit 0
    ;;
  *" -n oauth2-proxy delete referencegrants -l ${label}=12345 --ignore-not-found --wait=true --timeout=25s ")
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

# ---------------------------------------------------------------------------
# Fake curl. It splits its arguments into transfers at `--next`, as curl does, and answers each by
# its Host header, expanding the transfer's own `-w` format the way curl does, so the script's real
# parsing is exercised. A pair's answers come from the next line of pairs.txt:
#   <control code> <echoed source or -> <authz code> <authz size> <authz connects> [<location>]
# (an exhausted file answers 000 everywhere, as an unreachable gateway would). A control request
# sent alone with -S is the warm-up diagnosis: it prints curl-error (which names an address) to
# stderr and exits 35. It flags any request that is not to the load balancer URL, skips no
# certificate check, has no timeout, sends its own PROXY header, or sends the ExternalAuth request
# without the control request before it in the same invocation.
# ---------------------------------------------------------------------------
cat >"${fake_bin}/curl" <<'CURL'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/curl.log"
expand() {
  local f="$1"
  f="${f//%\{http_code\}/$2}"
  f="${f//%\{size_download\}/$3}"
  f="${f//%\{num_connects\}/$4}"
  f="${f//%\{redirect_url\}/$5}"
  printf '%b' "${f}"
}
next_pair() {
  local count=0 line
  [[ -f "${FIXTURES}/pairs-calls" ]] && count="$(cat "${FIXTURES}/pairs-calls")"
  count=$((count + 1))
  printf '%s' "${count}" >"${FIXTURES}/pairs-calls"
  line="$(sed -n "${count}p" "${FIXTURES}/pairs.txt" 2>/dev/null)"
  printf '%s' "${line:-000 - 000 0 0}"
}
transfers=0
pair=''
run_transfer() {
  local host="$1" fmt="$2" null="$3" insecure="$4" bounded="$5" url="$6" show_error="$7"
  local c ip a size conns loc
  transfers=$((transfers + 1))
  [[ "${insecure}" -eq 1 ]] || touch "${FIXTURES}/CURL_VERIFIES_CERTS"
  [[ "${bounded}" -eq 1 ]] || touch "${FIXTURES}/CURL_UNBOUNDED"
  [[ "${url}" == "https://203.0.113.10/" ]] || touch "${FIXTURES}/CURL_WRONG_URL"
  case "${host}" in
    control.externalauth-probe.invalid)
      if [[ "${show_error}" -eq 1 ]]; then
        [[ -f "${FIXTURES}/curl-error" ]] || exit 0
        cat "${FIXTURES}/curl-error" >&2
        exit 35
      fi
      pair="$(next_pair)"
      read -r c ip _ <<<"${pair}"
      if [[ "${null}" -eq 0 && "${c}" != '000' ]]; then
        printf 'Hostname: whoami\n'
        [[ "${ip}" != '-' ]] && printf 'RemoteAddr: %s:43210\n' "${ip}"
      fi
      expand "${fmt}" "${c}" 45 1 ''
      ;;
    authz.externalauth-probe.invalid)
      if [[ -z "${pair}" ]]; then
        touch "${FIXTURES}/SPLIT_PAIR"
        pair='000 - 000 0 0'
      fi
      read -r _ _ a size conns loc <<<"${pair}"
      expand "${fmt}" "${a}" "${size}" "${conns}" "${loc:-}"
      ;;
    unrouted.externalauth-probe.invalid)
      expand "${fmt}" "$(cat "${FIXTURES}/guard" 2>/dev/null || printf '404')" 0 1 ''
      ;;
    *) touch "${FIXTURES}/CURL_UNKNOWN_HOST" ;;
  esac
}
host='' fmt='' null=0 insecure=0 bounded=0 url='' show_error=0
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --haproxy-protocol) touch "${FIXTURES}/CURL_SENT_PROXY_HEADER"; shift ;;
    --next)
      run_transfer "${host}" "${fmt}" "${null}" "${insecure}" "${bounded}" "${url}" "${show_error}"
      host='' fmt='' null=0 insecure=0 bounded=0 url='' show_error=0
      shift
      ;;
    -H) host="${2#Host: }"; shift 2 ;;
    -w) fmt="$2"; shift 2 ;;
    -o) [[ "$2" == /dev/null ]] && null=1; shift 2 ;;
    --max-time) bounded=1; shift 2 ;;
    -k) insecure=1; shift ;;
    --*) touch "${FIXTURES}/CURL_UNKNOWN_OPTION"; shift ;;
    -*)
      [[ "$1" == *k* ]] && insecure=1
      [[ "$1" == *S* ]] && show_error=1
      shift
      ;;
    *) url="$1"; shift ;;
  esac
done
run_transfer "${host}" "${fmt}" "${null}" "${insecure}" "${bounded}" "${url}" "${show_error}"
exit 0
CURL
chmod +x "${fake_bin}/curl"

readonly requests=20
readonly dex_redirect='https://dex.platform.example.test/auth?client_id=public-client'
# Ingress IPs (see ciliumnodes.json): prod-worker-1 and prod-worker-2 host the oauth2-proxy
# endpoints, so their pairs are same-node; prod-worker-3 and the control plane host none.
readonly ip_cp1='10.0.10.2'
readonly ip_w1='10.0.10.3'
readonly ip_w2='10.0.10.4'
readonly ip_w3='10.0.10.5'
readonly delivered="302 0 0 ${dex_redirect}"
readonly blackholed='000 0 0'

# gen_pairs <cross-answer> <same-answer> [<n-first-cross> <first-cross-answer> [<n-first-same> <first-same-answer>]]
# Writes a warm-up pair, then 10 cross-node pairs (prod-worker-3) and 10 same-node pairs
# (prod-worker-1). An answer is "<authz code> <size> <connects> [<location>]".
gen_pairs() {
  local cross="$1" same="$2" cross_n="${3:-0}" cross_first="${4:-}" same_n="${5:-0}" same_first="${6:-}" i
  {
    printf '200 %s %s\n' "${ip_w1}" "${delivered}"
    for ((i = 1; i <= 10; i++)); do
      if ((i <= cross_n)); then printf '200 %s %s\n' "${ip_w3}" "${cross_first}"; else printf '200 %s %s\n' "${ip_w3}" "${cross}"; fi
    done
    for ((i = 1; i <= 10; i++)); do
      if ((i <= same_n)); then printf '200 %s %s\n' "${ip_w1}" "${same_first}"; else printf '200 %s %s\n' "${ip_w1}" "${same}"; fi
    done
  } >"${fixtures}/pairs.txt"
}

# Replaces line <n> (1 = the warm-up pair) of pairs.txt.
set_pair() {
  awk -v n="$1" -v l="$2" 'NR == n { print l; next } { print }' "${fixtures}/pairs.txt" >"${fixtures}/pairs.tmp"
  mv "${fixtures}/pairs.tmp" "${fixtures}/pairs.txt"
}

# ---------------------------------------------------------------------------
# Base fixtures: the #2284 topology. Two oauth2-proxy endpoints on prod-worker-1 and prod-worker-2;
# the control backend on prod-worker-2.
# ---------------------------------------------------------------------------
reset_fixtures() {
  rm -rf "${fixtures}"
  mkdir -p "${fixtures}"
  : >"${fixtures}/calls.log"
  : >"${fixtures}/curl.log"
  : >"${fixtures}/leftovers.txt"
  : >"${fixtures}/leftover-grants.txt"

  cat >"${fixtures}/endpoints.json" <<'JSON'
{"items":[{"endpoints":[
 {"addresses":["10.244.22.235"],"nodeName":"prod-worker-1","targetRef":{"kind":"Pod","uid":"uid-pod-a"},"conditions":{"ready":true,"serving":true,"terminating":false}},
 {"addresses":["10.244.23.28"],"nodeName":"prod-worker-2","targetRef":{"kind":"Pod","uid":"uid-pod-b"},"conditions":{"ready":true,"serving":true,"terminating":false}}
]}]}
JSON

  cat >"${fixtures}/backend.json" <<'JSON'
{"items":[{"endpoints":[
 {"addresses":["10.244.23.77"],"nodeName":"prod-worker-2","targetRef":{"kind":"Pod","uid":"uid-pod-w"},"conditions":{"ready":true,"serving":true,"terminating":false}}
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

  cat >"${fixtures}/ciliumnodes.json" <<'JSON'
{"items":[
 {"metadata":{"name":"prod-control-plane-1"},"spec":{"ingress":{"ipv4":"10.0.10.2"}}},
 {"metadata":{"name":"prod-worker-1"},"spec":{"ingress":{"ipv4":"10.0.10.3"}}},
 {"metadata":{"name":"prod-worker-2"},"spec":{"ingress":{"ipv4":"10.0.10.4"}}},
 {"metadata":{"name":"prod-worker-3"},"spec":{"ingress":{"ipv4":"10.0.10.5"}}}
]}
JSON

  cat >"${fixtures}/datapath.json" <<'JSON'
{"kind":"List","items":[
 {"metadata":{"name":"cilium","generation":7},"status":{"observedGeneration":7,"desiredNumberScheduled":4,"updatedNumberScheduled":4,"numberAvailable":4,"numberReady":4}},
 {"metadata":{"name":"cilium-envoy","generation":3},"status":{"observedGeneration":3,"desiredNumberScheduled":4,"updatedNumberScheduled":4,"numberAvailable":4,"numberReady":4}}
]}
JSON

  # The Service publishes the public IPv4, a public IPv6 and the private address, as the Hetzner
  # load balancer does.
  cat >"${fixtures}/gateway.json" <<'JSON'
{"metadata":{"annotations":{"load-balancer.hetzner.cloud/uses-proxyprotocol":"true","load-balancer.hetzner.cloud/use-private-ip":"true"}},
 "spec":{"type":"LoadBalancer"},
 "status":{"loadBalancer":{"ingress":[{"ip":"203.0.113.10","ipMode":"VIP"},{"ip":"2001:db8::1","ipMode":"VIP"},{"ip":"10.0.0.250","ipMode":"VIP"}]}}}
JSON

  cat >"${fixtures}/routes.json" <<'JSON'
{"items":[
 {"metadata":{"name":"externalauth-probe-control-12345"},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}},
 {"metadata":{"name":"externalauth-probe-authz-12345"},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}}
]}
JSON

  gen_pairs "${delivered}" "${delivered}"
}

run_probe_with() {
  local probe="$1"
  shift
  set +e
  output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" PROBE_POLL_SECONDS=0 PROBE_ROUTE_WAIT_SECONDS=1 \
    PROBE_WARMUP_PAUSE_SECONDS=0 GITHUB_ACTIONS=true GITHUB_STEP_SUMMARY='' bash "${probe}" "$@" 2>&1)"
  rc=$?
  set -e
}

run_probe() {
  run_probe_with "${script}" "$@"
}

run_default() {
  run_probe --context admin@prod --run-id "${run_id}" --requests "${requests}" --timeout 1
}

# Invariants that hold for EVERY run.
require_safe_surface() {
  local f
  [[ ! -e "${fixtures}/UNEXPECTED_CONTEXT" ]] || fail 'a call used another kube context'
  [[ ! -e "${fixtures}/FORBIDDEN_VERB" ]] || fail 'the probe issued a forbidden verb (exec, patch, edit, replace, scale, cordon, drain or logs)'
  [[ ! -e "${fixtures}/UNEXPECTED_CALL" ]] || fail 'the probe issued a kubectl call outside its pinned surface'
  for f in CURL_VERIFIES_CERTS CURL_UNBOUNDED CURL_WRONG_URL CURL_SENT_PROXY_HEADER SPLIT_PAIR CURL_UNKNOWN_HOST CURL_UNKNOWN_OPTION; do
    [[ ! -e "${fixtures}/${f}" ]] || fail "a curl request broke its pinned shape (${f})"
  done
  if grep -Fvq -- '--context admin@prod --request-timeout=30s ' "${fixtures}/calls.log"; then
    fail 'a kubectl call was not bounded by --request-timeout'
  fi
  if calls | grep -E ' delete ' | grep -Fvq -- "-l ${label}=${run_id} "; then
    fail 'a delete was not scoped to this run'"'"'s label'
  fi
  # The load balancer address appears only in the mask command, which the runner never prints.
  if grep -F -- "${lb_address}" <<<"${output}" | grep -Fvxq -- "::add-mask::${lb_address}"; then
    fail 'the load balancer address reached the log outside its mask command'
  fi
  if [[ -s "${fixtures}/curl.log" ]]; then
    [[ "$(head -n 1 <<<"${output}")" == "::add-mask::${lb_address}" ]] ||
      fail 'the load balancer address must be masked before any other output'
  fi
  refute_text 'prod-worker' 'a node name reached the log'
  refute_text 'prod-control-plane' 'a node name reached the log'
  refute_text 'host-w' 'a hostname label reached the log'
  refute_text '10.0.0.' 'a node or load balancer private address reached the log'
  refute_text '10.0.10.' 'an ingress IP reached the log'
  refute_text '10.244.' 'a pod address reached the log'
  refute_text '2001:db8' 'a load balancer IPv6 address reached the log'
  refute_text '198.51.100.' 'an error-text address reached the log'
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
  [[ ! -s "${fixtures}/curl.log" ]] || fail 'the probe sent requests on a path that must refuse first'
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
# The fixture holds 20 pairs, so pairs 21-50 find no gateway: the boundary run reached the requests.
require_inconclusive 'the budget boundary itself (250) must be accepted and reach the cluster' 'plain control route did not answer every time'
pass 'the requests x timeout budget is capped at exactly 250 seconds'

reset_fixtures
run_probe --context admin@prod --run-id 0123
require_rc 1 'a run id with a leading zero must be a usage error'
reset_fixtures
run_probe --context admin@prod --run-id 'abc'
require_rc 1 'a run id that is not a number must be a usage error'
[[ ! -s "${fixtures}/calls.log" ]] || fail 'a usage error still called kubectl'
pass 'the run id is decimal digits only'

# ---------------------------------------------------------------------------
# FIXED, and what it created and sent.
# ---------------------------------------------------------------------------
reset_fixtures
run_default
require_rc 0 'every request delivered must be conclusive'
require_text 'VERDICT: FIXED' 'all-delivered must read FIXED'
require_text 'cross-node pairs: 10 — ExternalAuth 10 delivered / 0 lost / 0 other' 'the cross-node group was not counted'
require_text 'same-node pairs:  10 — ExternalAuth 10 delivered / 0 lost / 0 other' 'the same-node group was not counted'
require_safe_surface
require_cleaned_up
applied="$(cat "${fixtures}/applied.yaml")"
[[ "$(grep -c 'type: ExternalAuth' <<<"${applied}")" -eq 1 ]] || fail 'exactly one route must carry the ExternalAuth filter'
grep -Fq -- '- control.externalauth-probe.invalid' <<<"${applied}" || fail 'the control route hostname is not the fixed .invalid name'
grep -Fq -- '- authz.externalauth-probe.invalid' <<<"${applied}" || fail 'the ExternalAuth route hostname is not the fixed .invalid name'
if grep -Fq -- '- unrouted.externalauth-probe.invalid' <<<"${applied}"; then
  fail 'the guard hostname must stay unrouted'
fi
[[ "$(grep -c 'sectionName: https$' <<<"${applied}")" -eq 2 ]] || fail 'both probe routes must attach to the https listener (the http listener admits only its own namespace)'
[[ "$(grep -c "${label}: \"12345\"" <<<"${applied}")" -eq 3 ]] || fail 'every created object must carry the run label'
# Top-level kinds only: the ReferenceGrant's `to:` entry legitimately names `kind: Service`.
created_kinds="$(grep -E '^kind: ' <<<"${applied}" | sort | uniq -c | awk '{print $1 "x" $3}' | paste -sd' ' -)"
[[ "${created_kinds}" == '2xHTTPRoute 1xReferenceGrant' ]] ||
  fail "the probe created a kind set outside its declared one (got: ${created_kinds})"
# One warm-up pair, the requested pairs, and the guard: every pair is ONE invocation carrying both
# routes, so the ExternalAuth request reuses the control request's connection.
[[ "$(wc -l <"${fixtures}/curl.log" | tr -d ' ')" -eq $((1 + requests + 1)) ]] || fail 'the probe did not send exactly the warm-up pair, the requested pairs and the guard'
[[ "$(grep -c -- '--next' "${fixtures}/curl.log")" -eq $((1 + requests)) ]] || fail 'every pair must be one curl invocation joined by --next'
pass 'FIXED: all cross-node pairs delivered; only two routes and a grant created, each pair one invocation, cleaned up'

# Pairs are grouped by the node whose Envoy answered, not by count: pairs on the other endpoint node
# (prod-worker-2) are same-node, and pairs on the control plane are cross-node.
reset_fixtures
gen_pairs "${delivered}" "${delivered}"
for n in 2 3 4 5 6; do set_pair "${n}" "200 ${ip_cp1} ${delivered}"; done
for n in 12 13 14 15 16; do set_pair "${n}" "200 ${ip_w2} ${blackholed}"; done
run_default
require_text 'cross-node pairs: 10 — ExternalAuth 10 delivered' 'control-plane pairs must count as cross-node'
require_text 'same-node pairs:  10 — ExternalAuth 5 delivered / 5 lost' 'pairs on the second endpoint node must count as same-node'
require_inconclusive 'same-node loss without cross-node loss is not the pattern' 'not the #2284 pattern'
pass 'pairs are grouped by the node the echoed source names'

# ---------------------------------------------------------------------------
# FAULT-PERSISTS and its one-fixture neighbours.
# ---------------------------------------------------------------------------
reset_fixtures
gen_pairs "${blackholed}" "${delivered}" 0 '' 5 "${blackholed}"
# A complete black-hole still warms up: the warm-up's ExternalAuth answer is a timeout.
set_pair 1 "200 ${ip_w3} ${blackholed}"
run_default
require_rc 0 'a complete cross-node black-hole with a healthy control must be conclusive'
require_text 'VERDICT: FAULT-PERSISTS' 'a complete cross-node black-hole must read FAULT-PERSISTS'
require_text '10 of 10 cross-node' 'the lost count must be reported'
require_safe_surface
require_cleaned_up
pass 'FAULT-PERSISTS: a complete cross-node black-hole while the control route answers, and a black-hole warms up'
readonly fault_pairs="${work_dir}/fault-pairs.txt"
cp "${fixtures}/pairs.txt" "${fault_pairs}"

reset_fixtures
gen_pairs "${delivered}" "${delivered}" 9 "${blackholed}" 5 "${blackholed}"
run_default
require_text 'VERDICT: FAULT-PERSISTS' '9 of 10 lost meets the 90% bar'
reset_fixtures
gen_pairs "${delivered}" "${delivered}" 8 "${blackholed}" 5 "${blackholed}"
run_default
require_inconclusive '8 of 10 lost is below the 90% bar' 'does not match the #2284 pattern'
pass 'FAULT-PERSISTS needs at least 90% cross-node loss (9 of 10 yes, 8 of 10 no)'

reset_fixtures
gen_pairs "${delivered}" "${delivered}" 1 "${blackholed}"
run_default
require_inconclusive 'one transient timeout must not be conclusive either way' 'does not match the #2284 pattern'
pass 'a single lost request is INCONCLUSIVE, not FAULT-PERSISTS'

reset_fixtures
gen_pairs "${blackholed}" "${blackholed}"
run_default
require_inconclusive 'no delivery anywhere means the route or backend is broken, not #2284' 'even on a node with a local endpoint'
reset_fixtures
gen_pairs "${delivered}" "${delivered}" 9 "${blackholed}" 9 "${blackholed}"
run_default
require_inconclusive 'cross-node loss not above same-node loss is not the #2284 pattern' 'does not match the #2284 pattern'
pass 'FAULT-PERSISTS needs same-node delivery and cross-node loss above same-node loss'

reset_fixtures
gen_pairs '403 0 0' "${delivered}" 0 '' 5 "${blackholed}"
run_default
require_text 'VERDICT: FAULT-PERSISTS' 'an EMPTY 403 is Envoy'"'"'s ext_authz error and counts as lost'
for code in 502 503 504; do
  reset_fixtures
  gen_pairs "${code} 19 0" "${delivered}" 0 '' 5 "${blackholed}"
  run_default
  require_text 'VERDICT: FAULT-PERSISTS' "a ${code} on the ExternalAuth route counts as lost"
done
pass 'an empty 403 and 502/503/504 count as lost'

reset_fixtures
gen_pairs "${delivered}" "${delivered}" 1 '500 12 0'
run_default
require_inconclusive 'a 500 came from a backend that answered, so it is not a lost subrequest' 'status codes: 500'
pass 'a 500 is other, not lost, and its status code is printed'

reset_fixtures
gen_pairs "${delivered}" "${delivered}" 1 '403 1480 0'
run_default
require_inconclusive 'a 403 WITH a body is not attributable' 'status codes: 403'
pass 'a 403 with a body is other, not lost'

reset_fixtures
gen_pairs "${delivered}" "${delivered}" 1 '302 0 0 https://evil.example.test/login'
run_default
require_inconclusive 'a redirect that is not the Dex login is not proof oauth2-proxy answered' 'status codes: 302'
refute_text 'evil.example.test' 'a redirect URL reached the log'
pass 'a 302 to anywhere but the Dex login is other, not delivered'

reset_fixtures
gen_pairs '401 21 0' "${delivered}"
run_default
require_text 'VERDICT: FIXED' 'a 401 is only ever produced by oauth2-proxy, so it counts as delivered'
pass 'a 401 counts as delivered'

reset_fixtures
gen_pairs "${blackholed}" "${delivered}" 0 '' 8 "${blackholed}"
run_default
require_inconclusive 'node-independent loss (cross 10, same 8 of 10) is not the #2284 pattern' 'does not match the #2284 pattern'
reset_fixtures
gen_pairs "${blackholed}" "${delivered}" 0 '' 6 "${blackholed}"
run_default
require_text 'VERDICT: FAULT-PERSISTS' 'same-node loss within 15 points of the remote-endpoint share (6 of 10 vs 50%) still separates'
pass 'FAULT-PERSISTS needs same-node loss near the remote-endpoint share, well below cross-node loss'

# The lower side of the band: near-total cross-node loss while same-node pairs, half of whose calls
# go to a remote endpoint, lose almost nothing. Their remote calls are being delivered, which
# contradicts a cross-node black-hole.
reset_fixtures
gen_pairs "${blackholed}" "${delivered}" 0 '' 1 "${blackholed}"
run_default
require_inconclusive 'same-node loss far below the remote-endpoint share (1 of 10 vs 50%) is not the #2284 pattern' 'does not match the #2284 pattern'
reset_fixtures
gen_pairs "${blackholed}" "${delivered}" 0 '' 4 "${blackholed}"
run_default
require_text 'VERDICT: FAULT-PERSISTS' 'same-node loss 10 points below the remote-endpoint share (4 of 10 vs 50%) is inside the band'
pass 'FAULT-PERSISTS also rejects same-node loss more than 15 points below the remote-endpoint share'

# ---------------------------------------------------------------------------
# The classifier is what these fixtures test: two broken copies of it must not reach the verdict
# the intact script reaches on the FAULT-PERSISTS fixture.
# ---------------------------------------------------------------------------
mutate() {
  local name="$1" from="$2" to="$3" mutant="${work_dir}/mutant-$1.sh"
  grep -Fq -- "${from}" "${script}" || fail "the ${name} mutation no longer matches the script"
  awk -v from="${from}" -v to="${to}" '{ i = index($0, from); if (i) $0 = substr($0, 1, i - 1) to substr($0, i + length(from)); print }' \
    "${script}" >"${mutant}"
  reset_fixtures
  cp "${fault_pairs}" "${fixtures}/pairs.txt"
  run_probe_with "${mutant}" --context admin@prod --run-id "${run_id}" --requests "${requests}" --timeout 1
  refute_text 'VERDICT: FAULT-PERSISTS' "a classifier with ${name} still read FAULT-PERSISTS, so the fixtures do not test it"
}
mutate 'swapped groups' 'g = (node in local_eps) ? "same" : "cross"' 'g = (node in local_eps) ? "cross" : "same"'
mutate 'timeouts not counted as lost' 'code == "000" || ' ''
pass 'a classifier with swapped groups, or that does not count a timeout as lost, fails the verdict fixture'

# ---------------------------------------------------------------------------
# Attribution: a pair counts only when its echoed source is one node's ingress IP and the
# ExternalAuth request reused the connection.
# ---------------------------------------------------------------------------
reset_fixtures
set_pair 2 "200 ${ip_w3} 302 0 1 ${dex_redirect}"
run_default
require_inconclusive 'a pair whose ExternalAuth request opened a new connection cannot be attributed' 'too few pairs were attributed to each group (cross 9, same 10, unattributed 1'
reset_fixtures
set_pair 12 "200 - ${delivered}"
run_default
require_inconclusive 'a pair with no echoed source cannot be attributed' 'too few pairs were attributed to each group (cross 10, same 9, unattributed 1'
reset_fixtures
set_pair 12 "200 10.244.23.77 ${delivered}"
run_default
require_inconclusive 'a pair whose echoed source is no node'"'"'s ingress IP cannot be attributed' 'unattributed 1'
pass 'a pair on a new connection, or with no or an unknown echoed source, is left out of both groups'

reset_fixtures
gen_pairs "${blackholed}" "${delivered}" 0 '' 5 "${blackholed}"
printf '200 - %s\n' "${blackholed}" >>"${fixtures}/pairs.txt"
run_probe --context admin@prod --run-id "${run_id}" --requests 21 --timeout 1
require_text 'VERDICT: FAULT-PERSISTS' 'an unattributed lost pair must not count against either group'
require_text '1 unattributed' 'the unattributed pair must be reported'
pass 'unattributed pairs are reported and do not change the verdict'

# ---------------------------------------------------------------------------
# INCONCLUSIVE paths: the verdict must not be taken.
# ---------------------------------------------------------------------------
reset_fixtures
gen_pairs "${blackholed}" "${delivered}" 0 '' 5 "${blackholed}"
set_pair 3 "000 - ${blackholed}"
run_default
require_inconclusive 'a control failure must block the FAULT-PERSISTS verdict' 'plain control route did not answer every time'
require_cleaned_up
pass 'a single control-route failure makes cross-node loss unattributable'

reset_fixtures
printf '302\n' >"${fixtures}/guard"
run_default
require_inconclusive 'the unrouted hostname answering anything but 404 means the probe affected other traffic' 'with 302 instead of 404'
pass 'the unrouted-hostname guard must still get the listener no-route 404'

reset_fixtures
gen_pairs '200 45 0' "${delivered}"
run_default
require_inconclusive 'a 200 on the ExternalAuth route means the filter is not applied' 'status codes: 200'
pass 'an unfiltered 200 is INCONCLUSIVE'

reset_fixtures
run_probe --context admin@prod --run-id "${run_id}" --requests 9 --timeout 1
require_inconclusive 'a 9-pair run is a smoke run' 'smoke run and never conclusive'
pass 'fewer than 10 pairs is never conclusive'

reset_fixtures
{
  printf '200 %s %s\n' "${ip_w1}" "${delivered}"
  for ((i = 1; i <= 10; i++)); do printf '200 %s %s\n' "${ip_w3}" "${delivered}"; done
  for ((i = 1; i <= 9; i++)); do printf '200 %s %s\n' "${ip_w1}" "${delivered}"; done
  printf '200 %s %s\n' "${ip_w3}" "${delivered}"
} >"${fixtures}/pairs.txt"
run_default
require_inconclusive 'only 9 same-node pairs is too few' 'too few pairs were attributed to each group (cross 11, same 9, unattributed 0'
pass 'each group needs at least 10 attributed pairs'

reset_fixtures
: >"${fixtures}/pairs.txt"
printf 'curl: (35) Recv failure: Connection reset by peer in connection to 198.51.100.77:443\n' >"${fixtures}/curl-error"
run_default
require_inconclusive 'a failed warm-up must say how the connection failed' 'during warm-up (last answers control=000 authz=000, then one more request failed with curl=connection-reset)'
[[ "$(grep -c -- '--next' "${fixtures}/curl.log")" -eq 12 ]] || fail 'the warm-up must try exactly its 12 pairs'
refute_text 'PAIR' 'no pair may be measured before the routes answer'
require_cleaned_up
reset_fixtures
for ((i = 1; i <= 12; i++)); do printf '200 %s 404 0 0\n' "${ip_w1}"; done >"${fixtures}/pairs.txt"
run_default
require_inconclusive 'an unprogrammed ExternalAuth route (404) must fail the warm-up' 'last answers control=200 authz=404, then one more request failed with curl=none'
pass 'a failed warm-up names each route'"'"'s last answer and how the connection failed, never curl'"'"'s raw text'

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
rm "${fixtures}/backend.json"
run_default
require_inconclusive 'an unreadable control backend must be INCONCLUSIVE' 'could not read the control backend endpoints'
require_nothing_created
reset_fixtures
printf '%s\n' '{"items":[]}' >"${fixtures}/backend.json"
run_default
require_inconclusive 'a control backend without endpoints must be INCONCLUSIVE' 'control backend Service has no endpoints'
require_nothing_created
reset_fixtures
sed 's/"nodeName":"prod-worker-2"/"nodeName":"prod-worker-3"/' "${fixtures}/backend.json" >"${fixtures}/backend-after.json"
grep -Fq '"nodeName":"prod-worker-3"' "${fixtures}/backend-after.json" || fail 'fixture did not move the control backend mid-run'
run_default
require_inconclusive 'a control backend moving during the run must be INCONCLUSIVE' 'control backend endpoints changed during the run'
require_cleaned_up
pass 'the control backend placement is read before and after'

reset_fixtures
sed 's/"ipv4":"10.0.10.5"/"ipv4":""/' "${fixtures}/ciliumnodes.json" >"${fixtures}/ciliumnodes.tmp"
mv "${fixtures}/ciliumnodes.tmp" "${fixtures}/ciliumnodes.json"
grep -Fq '"ipv4":""' "${fixtures}/ciliumnodes.json" || fail 'fixture did not blank an ingress IP'
run_default
require_inconclusive 'a node without an ingress IP must be INCONCLUSIVE' 'has no Cilium ingress IP'
require_nothing_created
reset_fixtures
sed 's/"ipv4":"10.0.10.4"/"ipv4":"10.0.10.9"/' "${fixtures}/ciliumnodes.json" >"${fixtures}/ciliumnodes-after.json"
grep -Fq '"ipv4":"10.0.10.9"' "${fixtures}/ciliumnodes-after.json" || fail 'fixture did not change an ingress IP'
run_default
require_inconclusive 'an ingress IP change during the run must be INCONCLUSIVE' 'ingress IPs changed during the run'
require_cleaned_up
pass 'a missing or changing ingress IP is INCONCLUSIVE'

# ---------------------------------------------------------------------------
# The load balancer the requests enter through.
# ---------------------------------------------------------------------------
reset_fixtures
sed 's/uses-proxyprotocol":"true"/uses-proxyprotocol":"false"/' "${fixtures}/gateway.json" >"${fixtures}/gateway.tmp"
mv "${fixtures}/gateway.tmp" "${fixtures}/gateway.json"
grep -Fq 'uses-proxyprotocol":"false"' "${fixtures}/gateway.json" || fail 'fixture did not turn the PROXY header off'
run_default
require_inconclusive 'a load balancer that sends no PROXY header cannot reach the listeners' 'does not declare that it sends the PROXY header'
require_nothing_created
reset_fixtures
sed 's/{"ip":"203.0.113.10","ipMode":"VIP"},//' "${fixtures}/gateway.json" >"${fixtures}/gateway.tmp"
mv "${fixtures}/gateway.tmp" "${fixtures}/gateway.json"
if grep -Fq '203.0.113.10' "${fixtures}/gateway.json"; then fail 'fixture did not drop the public address'; fi
run_default
require_inconclusive 'a load balancer with only a private IPv4 and an IPv6 address has nothing to send to' 'exactly one public IPv4 address'
require_nothing_created
reset_fixtures
sed 's/{"ip":"203.0.113.10","ipMode":"VIP"}/{"ip":"203.0.113.10"},{"ip":"203.0.113.11"}/' "${fixtures}/gateway.json" >"${fixtures}/gateway.tmp"
mv "${fixtures}/gateway.tmp" "${fixtures}/gateway.json"
run_default
require_inconclusive 'two public IPv4 addresses are ambiguous' 'exactly one public IPv4 address'
require_nothing_created
reset_fixtures
rm "${fixtures}/gateway.json"
run_default
require_inconclusive 'an unreadable gateway Service must be INCONCLUSIVE' 'could not read the gateway load balancer Service'
require_nothing_created
pass 'the load balancer must send the PROXY header and publish exactly one public IPv4 address'

reset_fixtures
touch "${fixtures}/apply-fails"
run_default
require_inconclusive 'a failed apply must be INCONCLUSIVE' 'could not create the ExternalAuth probe route'
require_cleaned_up
pass 'a failed apply still runs cleanup'

reset_fixtures
printf '%s\n' '{"items":[{"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"False"}]}]}},{"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}}]}' >"${fixtures}/routes.json"
run_default
require_inconclusive 'a route with unresolved references must stop before any request' 'not accepted with resolved references'
[[ ! -s "${fixtures}/curl.log" ]] || fail 'requests were sent although the routes were not ready'
require_cleaned_up
pass 'unresolved route references stop the run before any request is sent'

reset_fixtures
printf '%s\n' '{"items":[{"metadata":{"name":"externalauth-probe-authz-12345"},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True","reason":"Accepted"},{"type":"ResolvedRefs","status":"False","reason":"RefNotPermitted","message":"backend 10.0.10.7 on prod-worker-1 not permitted"}]}]}}]}' >"${fixtures}/routes.json"
run_default
require_inconclusive 'a held-back ExternalAuth route must say which condition held it' 'ExternalAuth route: ResolvedRefs=False (RefNotPermitted)'
refute_text 'not permitted' 'a condition message reached the log'
refute_text 'Accepted=True' 'a satisfied condition was reported as a blocker'
require_cleaned_up
pass 'an INCONCLUSIVE route wait names the blocking condition and reason, never its message'

reset_fixtures
printf '%s\n' '{"items":[{"metadata":{"name":"externalauth-probe-authz-12345"}}]}' >"${fixtures}/routes.json"
run_default
require_inconclusive 'a route the gateway never answered must say so' 'ExternalAuth route: no Accepted or ResolvedRefs condition yet'
pass 'a route with no status is reported as unanswered, not as blocked'

reset_fixtures
run_default
require_rc 0 'the ordering fixture run must still be conclusive'
first_route="$(awk '/^kind: HTTPRoute$/ {r = 1; next} r && /^  name: / {print $2; exit}' "${fixtures}/applied.yaml")"
[[ "${first_route}" == 'externalauth-probe-authz-12345' ]] ||
  fail "the ExternalAuth route must be created before the control route (first route: ${first_route})"
pass 'the ExternalAuth route is created before the control route'

reset_fixtures
printf '%s\n' '{"items":[{"metadata":{"name":"externalauth-probe-authz-12345"},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}},{"metadata":{"name":"externalauth-probe-control-12345"},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"False"}]}]}}]}' >"${fixtures}/routes.json"
run_default
require_inconclusive 'a control route that never resolves must stop before any request' 'control probe route was not accepted'
[[ ! -s "${fixtures}/curl.log" ]] || fail 'requests were sent although the control route was not ready'
require_cleaned_up
pass 'a control route that never resolves stops the run before any request is sent'

reset_fixtures
touch "${fixtures}/delete-fails"
run_default
require_rc 4 'a failed cleanup must override even a conclusive verdict'
require_text 'VERDICT: FIXED' 'the verdict itself should still be printed'
require_text 'CLEANUP: FAILED' 'the failed cleanup must be reported'
[[ -e "${fixtures}/deleted-grants" ]] || fail 'a failed whoami delete must not skip the ReferenceGrant delete'
require_safe_surface
pass 'a failed cleanup exits 4 even after a conclusive verdict, and still attempts every delete'

reset_fixtures
set +e
output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" PROBE_POLL_SECONDS=0 PROBE_ROUTE_WAIT_SECONDS=1 \
  PROBE_WARMUP_PAUSE_SECONDS=0 GITHUB_ACTIONS='' GITHUB_STEP_SUMMARY='' bash "${script}" \
  --context admin@prod --run-id "${run_id}" --requests "${requests}" --timeout 1 2>&1)"
rc=$?
set -e
require_text 'VERDICT: FIXED' 'outside GitHub Actions the probe must still run'
refute_text "${lb_address}" 'outside GitHub Actions the load balancer address must not be printed at all'
pass 'outside GitHub Actions nothing prints the load balancer address'

# ---------------------------------------------------------------------------
# Workflow shape.
# ---------------------------------------------------------------------------
wf="$(cat "${workflow}")"
output="${wf}"
rc=0
# The trigger key is quoted ("on":) so YAML 1.1 readers keep it a string, not `true`.
on_block="$(awk '/^"?on"?:/{f=1; next} f&&/^[a-z]/{exit} f' "${workflow}")"
[ -n "${on_block}" ] || fail 'the workflow trigger block could not be read'
grep -Fq 'workflow_dispatch:' <<<"${on_block}" || fail 'the workflow must be dispatch-triggered'
if grep -Eq '^[[:space:]]*(schedule|pull_request|pull_request_target|push|merge_group|workflow_run):' <<<"${on_block}"; then
  fail 'the workflow must have no trigger other than workflow_dispatch'
fi
# Every structural assertion below is anchored to EXECUTABLE workflow syntax. The workflow's header
# comments describe these same controls in prose, so an unanchored match could pass on a comment
# after the control itself was removed or moved.
line_of() {
  grep -nE -- "$1" "${workflow}" | head -1 | cut -d: -f1 || true
}
guard_line="$(line_of "^          if \[\[ \"\\\$\{RUN_REF\}\" != 'refs/heads/main' \]\]; then$")"
checkout_line="$(line_of '^        uses: actions/checkout@[0-9a-f]{40}')"
[[ -n "${guard_line}" && -n "${checkout_line}" && "${guard_line}" -lt "${checkout_line}" ]] ||
  fail 'the main-branch guard must run before checkout'
confirm_line="$(line_of "^          if \[\[ \"\\\$\{CONFIRM\}\" != 'probe-production-externalauth' \]\]; then$")"
kubeconfig_line="$(line_of '^          KUBE_CONFIG: \$\{\{ secrets\.KUBE_CONFIG \}\}$')"
[[ -n "${confirm_line}" && -n "${kubeconfig_line}" && "${confirm_line}" -lt "${kubeconfig_line}" ]] ||
  fail 'the confirmation phrase must be checked before the kubeconfig is restored'
grep -Eq '^  group: prod-deploy$' <<<"${wf}" || fail 'the workflow must serialise on the prod-deploy lock'
grep -Eq '^    environment: prod$' <<<"${wf}" || fail 'the workflow must use the prod environment'
grep -Eq '^      contents: read([[:space:]]+#.*)?$' <<<"${wf}" || fail 'the job token must be read-only'
if grep -Eq '^      [a-z-]+: write' <<<"${wf}"; then
  fail 'the job token must not be granted any write scope'
fi
grep -Eq '^permissions: \{\}$' <<<"${wf}" || fail 'the workflow must default to no permissions'
require_text '--context admin@prod --run-id "${RUN_ID}"' 'the workflow must pin the context and pass the run id'
require_text 'RUN_ID: ${{ github.run_id }}' 'the run id must come from github.run_id through env'
refute_text 'run: ./scripts/probe-cilium-externalauth-crossnode.sh --context admin@prod --run-id "${{' 'inputs must not be interpolated into run:'
job_minutes="$(sed -n 's/^    timeout-minutes: \([0-9][0-9]*\)$/\1/p' "${workflow}")"
[[ -n "${job_minutes}" ]] || fail 'the job must set timeout-minutes'
# Worst case the script allows (timeout 10, requests x timeout = 250, warm-up 12):
#   route wait (120s wall clock) + one in-flight 30s request past its deadline
#   + warm-up: 12 pairs of two requests at the timeout, a 1s pause each, and one diagnosis request
#   + the pairs: 2 requests each at the timeout, so 2 x 250
#   + the guard request
#   + ~25 other bounded kubectl calls (topology before and after, load balancer, leftovers, applies,
#     cleanup) at 30s
#   + 10 minutes of job setup and slack.
worst_seconds=$((120 + 35 + (12 * (2 * 10 + 1) + 10) + 2 * 250 + 10 + 25 * 30 + 600))
grep -Fq 'readonly route_wait_seconds="${PROBE_ROUTE_WAIT_SECONDS:-120}"' "${script}" || fail 'the route wait bound the timeout was sized for changed'
grep -Fq "readonly request_timeout='30s'" "${script}" || fail 'the request timeout the job timeout was sized for changed'
grep -Fq 'readonly warmup_pause_seconds="${PROBE_WARMUP_PAUSE_SECONDS:-1}"' "${script}" || fail 'the warm-up pause the timeout was sized for changed'
((job_minutes * 60 >= worst_seconds)) ||
  fail "timeout-minutes (${job_minutes}) is shorter than the script's worst case (${worst_seconds}s)"
grep -Fq 'readonly max_request_seconds=250' "${script}" || fail 'the budget cap the timeout was sized for changed'
grep -Fq 'readonly warmup_attempts=12' "${script}" || fail 'the warm-up bound the timeout was sized for changed'
default_requests="$(awk '/^      requests:$/{f=1} f && /^        default: /{gsub(/[^0-9]/, ""); print; exit}' "${workflow}")"
if [[ -z "${default_requests}" ]] || ((default_requests * 5 > 250)); then
  fail 'the default request count must fit the budget at the workflow'"'"'s 5s timeout'
fi
pass 'the workflow is dispatch-only, guarded, serialised, least-privilege and sized for the worst case'
