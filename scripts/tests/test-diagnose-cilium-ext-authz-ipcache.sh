#!/usr/bin/env bash
# Pin the behaviour of scripts/diagnose-cilium-ext-authz-ipcache.sh.
#
# WHY THIS EXISTS. The diagnostic decides whether #2284's precondition is still present on the
# deployed Cilium, and every one of its mistakes is silent: a FAULT-PERSISTS read off a CiliumInternalIP
# entry, a PLAUSIBLY-FIXED read off a pod IP that merely shares a prefix, or a verdict taken while a
# replica was mid-rollout would all look like a clean answer. So the conclusive verdicts each have a
# control that differs in exactly one fixture, and every INCONCLUSIVE path is exercised on purpose.
#
# It also pins the two properties that make the workflow safe to dispatch: the script issues only
# `get` and one exact `exec cilium-dbg bpf ipcache list`, and it prints no address, node name or pod
# name into a public log.
#
# kubectl is faked from a fixture directory; no cluster, no secrets, no network. Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/diagnose-cilium-ext-authz-ipcache.sh"

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
# Fake kubectl. Records every invocation, serves fixtures, and fails loudly on anything the
# diagnostic must never do. A failed call writes an address to stderr, the way a real
# connection error does, so the leak assertions below also cover the error paths.
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
serve() {
  if [[ -f "${FIXTURES}/$1" ]]; then
    cat "${FIXTURES}/$1"
  else
    printf 'error: dial tcp 198.51.100.9:6443: connect: connection refused\n' >&2
    exit 1
  fi
}
case "${args}" in
  *" -n kube-system exec "*)
    if [[ "${args}" != *" -c cilium-agent -- cilium-dbg bpf ipcache list " ]]; then
      touch "${FIXTURES}/UNEXPECTED_EXEC"
      exit 1
    fi
    serve ipcache.txt
    ;;
  *" -n oauth2-proxy get pods -l app.kubernetes.io/name=oauth2-proxy,app.kubernetes.io/instance=oauth2-proxy -o json ") serve oauth2-pods.json ;;
  *" -n kube-system get pods -l k8s-app=cilium -o json ") serve cilium-pods.json ;;
  *" get nodes -o json ") serve nodes.json ;;
  *)
    touch "${FIXTURES}/UNEXPECTED_CALL"
    exit 1
    ;;
esac
FAKE
chmod +x "${fake_bin}/kubectl"

# ---------------------------------------------------------------------------
# Base fixtures: the #2284 topology. Two oauth2-proxy replicas on prod-worker-1 and prod-worker-2;
# Cilium agents on every node; the agent that must be chosen is the one on prod-control-plane-1
# (first node, sorted, hosting neither replica).
# ---------------------------------------------------------------------------
reset_fixtures() {
  rm -rf "${fixtures}"
  mkdir -p "${fixtures}"
  : >"${fixtures}/calls.log"

  cat >"${fixtures}/oauth2-pods.json" <<'JSON'
{"items":[
 {"metadata":{"name":"oauth2-proxy-7c9d-aaaaa"},"spec":{"nodeName":"prod-worker-1"},
  "status":{"phase":"Running","podIP":"10.244.22.235","podIPs":[{"ip":"10.244.22.235"}],
   "conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"name":"oauth2-proxy-7c9d-bbbbb"},"spec":{"nodeName":"prod-worker-2"},
  "status":{"phase":"Running","podIP":"10.244.23.28","podIPs":[{"ip":"10.244.23.28"}],
   "conditions":[{"type":"Ready","status":"True"}]}}
]}
JSON

  cat >"${fixtures}/nodes.json" <<'JSON'
{"items":[
 {"metadata":{"name":"prod-control-plane-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.2"},{"type":"ExternalIP","address":"203.0.113.2"},{"type":"Hostname","address":"prod-control-plane-1"}]}},
 {"metadata":{"name":"prod-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.3"},{"type":"ExternalIP","address":"203.0.113.3"}]}},
 {"metadata":{"name":"prod-worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.4"},{"type":"ExternalIP","address":"203.0.113.4"}]}},
 {"metadata":{"name":"prod-worker-3"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.5"},{"type":"ExternalIP","address":"203.0.113.5"}]}}
]}
JSON

  cat >"${fixtures}/cilium-pods.json" <<'JSON'
{"items":[
 {"metadata":{"name":"cilium-wrk1a"},"spec":{"nodeName":"prod-worker-1"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}},
 {"metadata":{"name":"cilium-wrk2b"},"spec":{"nodeName":"prod-worker-2"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}},
 {"metadata":{"name":"cilium-wrk3c"},"spec":{"nodeName":"prod-worker-3"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}},
 {"metadata":{"name":"cilium-cpl1d"},"spec":{"nodeName":"prod-control-plane-1"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}}
]}
JSON

  # The fault shape recorded on 1.20.0-pre.3: pods behind WireGuard, node addresses plaintext.
  # 10.244.22.1 is a remote CiliumInternalIP (identity=6, behind WireGuard) and must NOT vote.
  cat >"${fixtures}/ipcache.txt" <<'TXT'
IP PREFIX/ADDRESS   IDENTITY
10.244.22.235/32    identity=11566 encryptkey=255 tunnelendpoint=10.0.0.3 flags=hastunnel
10.244.23.28/32     identity=11566 encryptkey=255 tunnelendpoint=10.0.0.4 flags=hastunnel
10.0.0.3/32         identity=6 encryptkey=0 tunnelendpoint=0.0.0.0 flags=<none>
10.0.0.4/32         identity=6 encryptkey=0 tunnelendpoint=0.0.0.0 flags=<none>
10.0.0.5/32         identity=6 encryptkey=0 tunnelendpoint=0.0.0.0 flags=<none>
10.0.0.2/32         identity=1 encryptkey=0 tunnelendpoint=0.0.0.0 flags=<none>
10.244.22.1/32      identity=6 encryptkey=255 tunnelendpoint=10.0.0.3 flags=hastunnel
0.0.0.0/0           identity=2 encryptkey=0 tunnelendpoint=0.0.0.0 flags=<none>
TXT
}

# Rewrite the encryptkey of exactly the named ipcache entries.
set_ipcache_key() {
  local key="$1"
  shift
  local address
  for address in "$@"; do
    awk -v addr="${address}/32" -v key="${key}" '
      $1 == addr { sub(/encryptkey=[^ ]*/, "encryptkey=" key) } { print }
    ' "${fixtures}/ipcache.txt" >"${fixtures}/ipcache.tmp"
    mv "${fixtures}/ipcache.tmp" "${fixtures}/ipcache.txt"
  done
}

drop_ipcache_entry() {
  awk -v addr="$1/32" '$1 != addr' "${fixtures}/ipcache.txt" >"${fixtures}/ipcache.tmp"
  mv "${fixtures}/ipcache.tmp" "${fixtures}/ipcache.txt"
}

edit_json() {
  local file="$1" filter="$2"
  jq "${filter}" "${fixtures}/${file}" >"${fixtures}/${file}.tmp"
  mv "${fixtures}/${file}.tmp" "${fixtures}/${file}"
}

run_script() {
  set +e
  output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" GITHUB_STEP_SUMMARY='' \
    bash "${script}" "$@" 2>&1)"
  rc=$?
  set -e
}

# Every case: nothing but the four read-only calls, at most one exec, and no identifying data.
assert_safe() {
  local marker
  for marker in UNEXPECTED_CONTEXT UNEXPECTED_EXEC UNEXPECTED_CALL; do
    [[ ! -e "${fixtures}/${marker}" ]] || fail "the script issued a disallowed kubectl call (${marker})"
  done
  local execs
  execs="$(grep -c ' exec ' "${fixtures}/calls.log" || true)"
  [[ "${execs}" -le 1 ]] || fail "the script exec'd more than once (${execs})"
  local needle
  for needle in 10.244. 10.0.0. 203.0.113. 198.51.100. prod-worker prod-control-plane cilium-wrk cilium-cpl oauth2-proxy-7c9d; do
    refute_text "${needle}" "output leaked identifying data (${needle})"
  done
}

cases=0
pass() {
  cases=$((cases + 1))
  printf 'ok %s - %s\n' "${cases}" "$1"
}

# --- Conclusive verdicts, each with a one-fixture control ---------------------------------------

reset_fixtures
run_script --context admin@prod
require_rc 0 'fault shape must be conclusive'
require_text 'VERDICT: FAULT-PERSISTS' 'fault shape must report FAULT-PERSISTS'
require_text 'identity=6 node-address entries (decide): encryptkey=0: 3, encryptkey!=0: 0' 'node-address counts'
require_text 'identity=6 other entries (informational): encryptkey=0: 0, other: 1' 'CiliumInternalIP entry must be counted separately'
grep -q -- ' exec cilium-cpl1d -c cilium-agent ' "${fixtures}/calls.log" || fail 'must exec in the agent on the first node hosting neither replica'
assert_safe
pass 'fault shape reports FAULT-PERSISTS from the agent on a replica-free node'

reset_fixtures
set_ipcache_key 255 10.0.0.3 10.0.0.4 10.0.0.5
run_script --context admin@prod
require_rc 0 'fixed shape must be conclusive'
require_text 'VERDICT: PLAUSIBLY-FIXED' 'node addresses behind WireGuard must report PLAUSIBLY-FIXED'
refute_text 'FAULT-PERSISTS' 'fixed shape must not also report the fault'
assert_safe
pass 'control: the same fixture with encrypted node addresses reports PLAUSIBLY-FIXED'

# Negative control for the vote: identity=6 entries that are NOT node addresses never decide.
reset_fixtures
drop_ipcache_entry 10.0.0.3
drop_ipcache_entry 10.0.0.4
drop_ipcache_entry 10.0.0.5
set_ipcache_key 0 10.244.22.1
run_script --context admin@prod
require_rc 3 'no node-address entry must be inconclusive'
require_text 'VERDICT: INCONCLUSIVE' 'a plaintext non-node identity=6 entry alone must not conclude'
require_text 'proxy source is unobserved' 'reason must name the missing source'
assert_safe
pass 'negative control: a plaintext CiliumInternalIP entry cannot produce FAULT-PERSISTS'

# Negative control for exact matching: a pod IP that is a prefix of an ipcache entry is absent.
reset_fixtures
edit_json oauth2-pods.json '.items[0].status.podIP = "10.244.22.23" | .items[0].status.podIPs = [{"ip":"10.244.22.23"}]'
run_script --context admin@prod
require_rc 3 'prefix-only match must be inconclusive'
require_text 'oauth2-proxy pod IP #1 has no ipcache entry' 'addresses must match exactly, not by prefix'
assert_safe
pass 'negative control: 10.244.22.23 is not satisfied by an entry for 10.244.22.235'

# --- INCONCLUSIVE paths -------------------------------------------------------------------------

reset_fixtures
drop_ipcache_entry 10.244.23.28
run_script --context admin@prod
require_rc 3 'missing pod entry'
require_text 'oauth2-proxy pod IP #2 has no ipcache entry' 'missing pod entry reason'
assert_safe
pass 'a pod IP missing from the ipcache is INCONCLUSIVE'

reset_fixtures
set_ipcache_key 0 10.244.22.235
run_script --context admin@prod
require_rc 3 'pod not behind WireGuard'
require_text 'is not behind WireGuard' 'premise reason'
assert_safe
pass 'a pod entry not behind WireGuard is INCONCLUSIVE, not a verdict'

reset_fixtures
set_ipcache_key 255 10.0.0.5
run_script --context admin@prod
require_rc 3 'mixed node-address population'
require_text 'disagree on encryptkey' 'mixed reason'
assert_safe
pass 'a mixed identity=6 node-address population is INCONCLUSIVE'

reset_fixtures
awk '$1 == "10.0.0.4/32" { print $1, $2, $4, $5; next } { print }' "${fixtures}/ipcache.txt" >"${fixtures}/ipcache.tmp"
mv "${fixtures}/ipcache.tmp" "${fixtures}/ipcache.txt"
run_script --context admin@prod
require_rc 3 'unparseable node-address entry'
require_text 'no parseable encryptkey' 'unparseable reason'
assert_safe
pass 'an identity=6 node-address entry without encryptkey is INCONCLUSIVE'

reset_fixtures
edit_json cilium-pods.json '.items |= map(select(.spec.nodeName == "prod-worker-1" or .spec.nodeName == "prod-worker-2"))'
run_script --context admin@prod
require_rc 3 'no eligible agent'
require_text 'no Ready Cilium agent runs on a node hosting neither' 'no eligible agent reason'
if grep -q ' exec ' "${fixtures}/calls.log"; then
  fail 'must not exec in an agent on a replica node'
fi
assert_safe
pass 'agents only on replica nodes: INCONCLUSIVE with no exec'

reset_fixtures
edit_json cilium-pods.json '.items |= map(if .spec.nodeName == "prod-control-plane-1" then .status.containerStatuses[0].ready = false else . end)'
run_script --context admin@prod
require_rc 0 'next eligible agent'
grep -q -- ' exec cilium-wrk3c -c cilium-agent ' "${fixtures}/calls.log" || fail 'a not-Ready agent must be skipped for the next replica-free node'
assert_safe
pass 'a not-Ready agent is skipped in favour of the next replica-free node'

reset_fixtures
edit_json oauth2-pods.json '.items[1].status.phase = "Pending"'
run_script --context admin@prod
require_rc 3 'rollout in flight'
require_text 'rollout in flight' 'unsettled reason'
if grep -q ' exec ' "${fixtures}/calls.log"; then
  fail 'must not exec while a replica is unsettled'
fi
assert_safe
pass 'an unsettled oauth2-proxy replica is INCONCLUSIVE before any exec'

reset_fixtures
edit_json oauth2-pods.json '.items = []'
run_script --context admin@prod
require_rc 3 'no replicas'
require_text 'no oauth2-proxy pods were found' 'no replicas reason'
assert_safe
pass 'no oauth2-proxy pods is INCONCLUSIVE'

reset_fixtures
rm "${fixtures}/ipcache.txt"
run_script --context admin@prod
require_rc 3 'exec failure'
require_text 'the ipcache read in the Cilium agent failed' 'exec failure reason'
assert_safe
pass 'a failed exec is INCONCLUSIVE and its stderr is not echoed'

reset_fixtures
: >"${fixtures}/ipcache.txt"
run_script --context admin@prod
require_rc 3 'empty ipcache'
require_text 'the ipcache read returned nothing' 'empty ipcache reason'
assert_safe
pass 'an empty ipcache is INCONCLUSIVE'

reset_fixtures
rm "${fixtures}/nodes.json"
run_script --context admin@prod
require_rc 3 'node list failure'
require_text 'could not list the nodes' 'node list failure reason'
assert_safe
pass 'a failed node list is INCONCLUSIVE'

reset_fixtures
printf 'not json' >"${fixtures}/oauth2-pods.json"
run_script --context admin@prod
require_rc 3 'malformed pod list'
require_text 'did not parse' 'malformed pod list reason'
assert_safe
pass 'a malformed pod list is INCONCLUSIVE'

# --- Usage ---------------------------------------------------------------------------------------

reset_fixtures
run_script
require_rc 1 'missing context'
require_text '--context is required' 'missing context reason'
[[ ! -s "${fixtures}/calls.log" ]] || fail 'no kubectl call may happen without --context'
pass 'missing --context refuses before any kubectl call'

# --- Step summary --------------------------------------------------------------------------------

reset_fixtures
summary_file="${work_dir}/summary.md"
: >"${summary_file}"
set +e
output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" GITHUB_STEP_SUMMARY="${summary_file}" \
  bash "${script}" --context admin@prod 2>&1)"
rc=$?
set -e
require_rc 0 'summary case'
grep -Fq '**Verdict:** FAULT-PERSISTS' "${summary_file}" || fail 'the verdict must reach the step summary'
if grep -Eq '10\.|203\.0\.113\.|prod-' "${summary_file}"; then
  fail 'the step summary leaked identifying data'
fi
pass 'the verdict is written to the step summary without identifying data'

printf 'All %s cases passed.\n' "${cases}"
