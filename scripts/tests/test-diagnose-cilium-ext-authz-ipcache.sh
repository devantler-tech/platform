#!/usr/bin/env bash
# Pin the behaviour of scripts/diagnose-cilium-ext-authz-ipcache.sh.
#
# WHY THIS EXISTS. The diagnostic decides whether #2284's precondition is still present on the
# deployed Cilium, and every one of its mistakes is silent: a FAULT-PERSISTS read off a CiliumInternalIP
# entry, off node-address keys that are plaintext by configuration, or off a pod IP that merely
# shares a prefix; or a verdict taken while a replica was mid-rollout, from an agent on an old
# revision, or across replicas or a node set that changed during the read — all would look like a
# clean answer. So the conclusive verdicts each have a control that differs in exactly one fixture,
# and every INCONCLUSIVE path is exercised on purpose.
#
# It also pins the two properties that make the workflow safe to dispatch: the script issues only
# `get` and one exact `exec cilium-dbg bpf ipcache list`, and it prints no address, node name, UID,
# revision hash or pod name into a public log.
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
#
# The node list and the oauth2-proxy pod list are served per call: the first read returns the base
# fixture, and a later one returns the matching *-after.json when a case provides it (or fails when
# the matching *-after-fails marker exists), so a change during the read can be simulated.
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
# serve_per_call <base-name>: <base>.json first, then <base>-after.json / <base>-after-fails.
serve_per_call() {
  local base="$1" calls=0
  [[ -f "${FIXTURES}/${base}-calls" ]] && calls="$(cat "${FIXTURES}/${base}-calls")"
  calls=$((calls + 1))
  printf '%s' "${calls}" >"${FIXTURES}/${base}-calls"
  if [[ "${calls}" -ge 2 && -f "${FIXTURES}/${base}-after-fails" ]]; then
    printf 'error: dial tcp 198.51.100.9:6443: connect: connection refused\n' >&2
    exit 1
  fi
  if [[ "${calls}" -ge 2 && -f "${FIXTURES}/${base}-after.json" ]]; then
    serve "${base}-after.json"
  else
    serve "${base}.json"
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
  *" -n oauth2-proxy get pods -l app.kubernetes.io/name=oauth2-proxy,app.kubernetes.io/instance=oauth2-proxy -o json ") serve_per_call oauth2-pods ;;
  *" -n kube-system get pods -l k8s-app=cilium -o json ") serve cilium-pods.json ;;
  *" -n kube-system get daemonset cilium -o json ") serve daemonset.json ;;
  *" -n kube-system get controllerrevisions -o json ") serve controllerrevisions.json ;;
  *" -n kube-system get configmap cilium-config -o json ") serve cilium-config.json ;;
  *" get nodes -o json ") serve_per_call nodes ;;
  *)
    touch "${FIXTURES}/UNEXPECTED_CALL"
    exit 1
    ;;
esac
FAKE
chmod +x "${fake_bin}/kubectl"

readonly current_hash='7c9b8d6f5e'
readonly old_hash='5d8f7c6b9a'

# ---------------------------------------------------------------------------
# Base fixtures: the #2284 topology. Two oauth2-proxy replicas on prod-worker-1 and prod-worker-2;
# Cilium agents on every node, all on the DaemonSet's current revision; the agent that must be
# chosen is the one on prod-control-plane-1 (first node, sorted, hosting neither replica).
#
# NOTE: the base cilium-config has node encryption ON, the only configuration in which all-zero
# node-address keys can show the fault. Production runs with it OFF; those cases set it explicitly.
# ---------------------------------------------------------------------------
reset_fixtures() {
  rm -rf "${fixtures}"
  mkdir -p "${fixtures}"
  : >"${fixtures}/calls.log"

  cat >"${fixtures}/oauth2-pods.json" <<'JSON'
{"items":[
 {"metadata":{"name":"oauth2-proxy-7c9d-aaaaa","uid":"uid-pod-a"},"spec":{"nodeName":"prod-worker-1"},
  "status":{"phase":"Running","podIP":"10.244.22.235","podIPs":[{"ip":"10.244.22.235"}],
   "conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"name":"oauth2-proxy-7c9d-bbbbb","uid":"uid-pod-b"},"spec":{"nodeName":"prod-worker-2"},
  "status":{"phase":"Running","podIP":"10.244.23.28","podIPs":[{"ip":"10.244.23.28"}],
   "conditions":[{"type":"Ready","status":"True"}]}}
]}
JSON

  cat >"${fixtures}/nodes.json" <<'JSON'
{"items":[
 {"metadata":{"name":"prod-control-plane-1","uid":"uid-node-cp1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.2"},{"type":"ExternalIP","address":"203.0.113.2"},{"type":"Hostname","address":"prod-control-plane-1"}]}},
 {"metadata":{"name":"prod-worker-1","uid":"uid-node-w1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.3"},{"type":"ExternalIP","address":"203.0.113.3"}]}},
 {"metadata":{"name":"prod-worker-2","uid":"uid-node-w2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.4"},{"type":"ExternalIP","address":"203.0.113.4"}]}},
 {"metadata":{"name":"prod-worker-3","uid":"uid-node-w3"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.5"},{"type":"ExternalIP","address":"203.0.113.5"}]}}
]}
JSON

  cat >"${fixtures}/cilium-pods.json" <<JSON
{"items":[
 {"metadata":{"name":"cilium-wrk1a","labels":{"k8s-app":"cilium","controller-revision-hash":"${current_hash}"}},"spec":{"nodeName":"prod-worker-1"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}},
 {"metadata":{"name":"cilium-wrk2b","labels":{"k8s-app":"cilium","controller-revision-hash":"${current_hash}"}},"spec":{"nodeName":"prod-worker-2"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}},
 {"metadata":{"name":"cilium-wrk3c","labels":{"k8s-app":"cilium","controller-revision-hash":"${current_hash}"}},"spec":{"nodeName":"prod-worker-3"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}},
 {"metadata":{"name":"cilium-cpl1d","labels":{"k8s-app":"cilium","controller-revision-hash":"${current_hash}"}},"spec":{"nodeName":"prod-control-plane-1"},"status":{"phase":"Running","containerStatuses":[{"name":"cilium-agent","ready":true}]}}
]}
JSON

  cat >"${fixtures}/daemonset.json" <<'JSON'
{"metadata":{"name":"cilium","uid":"uid-ds-cilium","generation":7},
 "status":{"observedGeneration":7,"desiredNumberScheduled":4,"currentNumberScheduled":4,
  "updatedNumberScheduled":4,"numberAvailable":4,"numberReady":4}}
JSON

  # The old revision is listed FIRST so "current" must come from the highest revision number, not
  # list order. The foreign revision has a higher number but another owner, so it must be ignored.
  cat >"${fixtures}/controllerrevisions.json" <<JSON
{"items":[
 {"metadata":{"name":"cilium-${old_hash}","labels":{"k8s-app":"cilium","controller-revision-hash":"${old_hash}"},
   "ownerReferences":[{"kind":"DaemonSet","name":"cilium","uid":"uid-ds-cilium","controller":true}]},"revision":5},
 {"metadata":{"name":"cilium-${current_hash}","labels":{"k8s-app":"cilium","controller-revision-hash":"${current_hash}"},
   "ownerReferences":[{"kind":"DaemonSet","name":"cilium","uid":"uid-ds-cilium","controller":true}]},"revision":6},
 {"metadata":{"name":"cilium-envoy-0a1b2c3d4e","labels":{"controller-revision-hash":"0a1b2c3d4e"},
   "ownerReferences":[{"kind":"DaemonSet","name":"cilium-envoy","uid":"uid-ds-envoy","controller":true}]},"revision":9}
]}
JSON

  # As the 1.20.1 chart renders it with encryption.nodeEncryption: true.
  cat >"${fixtures}/cilium-config.json" <<'JSON'
{"metadata":{"name":"cilium-config","namespace":"kube-system"},
 "data":{"enable-wireguard":"true","encrypt-node":"true","routing-mode":"tunnel"}}
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

set_ipcache_identity() {
  local identity="$1" address="$2"
  awk -v addr="${address}/32" -v identity="${identity}" '
    $1 == addr { sub(/identity=[^ ]*/, "identity=" identity) } { print }
  ' "${fixtures}/ipcache.txt" >"${fixtures}/ipcache.tmp"
  mv "${fixtures}/ipcache.tmp" "${fixtures}/ipcache.txt"
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

# Write <base>-after.json as <base>.json transformed by a jq filter: the state seen after the exec.
nodes_after() {
  jq "$1" "${fixtures}/nodes.json" >"${fixtures}/nodes-after.json"
}

replicas_after() {
  jq "$1" "${fixtures}/oauth2-pods.json" >"${fixtures}/oauth2-pods-after.json"
}

run_script() {
  set +e
  output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" GITHUB_STEP_SUMMARY='' \
    bash "${script}" "$@" 2>&1)"
  rc=$?
  set -e
}

require_no_exec() {
  if grep -q ' exec ' "${fixtures}/calls.log"; then
    fail "$1"
  fi
}

# Every case: nothing but the read-only calls, at most one exec, and no identifying data.
assert_safe() {
  local marker
  for marker in UNEXPECTED_CONTEXT UNEXPECTED_EXEC UNEXPECTED_CALL; do
    [[ ! -e "${fixtures}/${marker}" ]] || fail "the script issued a disallowed kubectl call (${marker})"
  done
  local execs
  execs="$(grep -c ' exec ' "${fixtures}/calls.log" || true)"
  [[ "${execs}" -le 1 ]] || fail "the script exec'd more than once (${execs})"
  local needle
  for needle in 10.244. 10.0.0. 203.0.113. 198.51.100. prod-worker prod-control-plane cilium-wrk cilium-cpl \
    oauth2-proxy-7c9d uid-node uid-pod uid-ds "${current_hash}" "${old_hash}" 0a1b2c3d4e; do
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
require_text 'VERDICT: FAULT-PERSISTS' 'fault shape with node encryption on must report FAULT-PERSISTS'
require_text 'Cilium node encryption: on' 'node encryption must be reported'
require_text 'identity=6 node-address entries (decide): encryptkey=0: 3, encryptkey!=0: 0' 'node-address counts'
require_text 'identity=6 other entries (informational): encryptkey=0: 0, other: 1' 'CiliumInternalIP entry must be counted separately'
require_text 'Cilium DaemonSet: fully rolled out; selected agent is on the current revision' 'rollout must be verified'
require_text 'oauth2-proxy replicas: unchanged across the read' 'replicas must be re-verified'
require_text 'node topology: unchanged across the read' 'topology must be re-verified'
grep -q -- ' exec cilium-cpl1d -c cilium-agent ' "${fixtures}/calls.log" || fail 'must exec in the agent on the first node hosting neither replica'
[[ "$(grep -c ' get nodes -o json' "${fixtures}/calls.log")" -eq 2 ]] || fail 'the nodes must be listed exactly twice'
[[ "$(grep -c ' -n oauth2-proxy get pods ' "${fixtures}/calls.log")" -eq 2 ]] || fail 'the oauth2-proxy pods must be listed exactly twice'
exec_at="$(grep -n ' exec ' "${fixtures}/calls.log" | cut -d: -f1)"
last_nodes_at="$(grep -n ' get nodes -o json' "${fixtures}/calls.log" | tail -n 1 | cut -d: -f1)"
last_replicas_at="$(grep -n ' -n oauth2-proxy get pods ' "${fixtures}/calls.log" | tail -n 1 | cut -d: -f1)"
[[ "${last_nodes_at}" -gt "${exec_at}" ]] || fail 'the second node list must come after the exec'
[[ "${last_replicas_at}" -gt "${exec_at}" ]] || fail 'the second oauth2-proxy pod list must come after the exec'
assert_safe
pass 'node encryption on + all zero reports FAULT-PERSISTS, with replicas and topology re-read after the exec'

reset_fixtures
set_ipcache_key 255 10.0.0.3 10.0.0.4 10.0.0.5
run_script --context admin@prod
require_rc 0 'fixed shape must be conclusive'
require_text 'VERDICT: PLAUSIBLY-FIXED' 'node addresses behind WireGuard must report PLAUSIBLY-FIXED'
require_text 'Cilium node encryption: on' 'node encryption on in the fixed control'
refute_text 'FAULT-PERSISTS' 'fixed shape must not also report the fault'
assert_safe
pass 'control: node encryption on + non-zero node-address keys reports PLAUSIBLY-FIXED'

# Negative control for the vote: identity=6 entries that are NOT node addresses never decide.
reset_fixtures
drop_ipcache_entry 10.0.0.3
drop_ipcache_entry 10.0.0.4
drop_ipcache_entry 10.0.0.5
set_ipcache_key 0 10.244.22.1
run_script --context admin@prod
require_rc 3 'no node-address entry must be inconclusive'
require_text 'VERDICT: INCONCLUSIVE' 'a plaintext non-node identity=6 entry alone must not conclude'
require_text 'has no identity=6 entry for any of its addresses' 'reason must name the uncovered source'
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

# --- Node encryption: all-zero keys conclude the fault only when node encryption is ON -----------

# The production shape: the chart omits encrypt-node when nodeEncryption is false.
reset_fixtures
edit_json cilium-config.json 'del(.data["encrypt-node"])'
run_script --context admin@prod
require_rc 3 'node encryption off (absent key) + all zero'
require_text 'Cilium node encryption: off' 'an absent encrypt-node key must read as off'
require_text 'Cilium node encryption is off, so that is the expected state and cannot show the fault' 'off reason'
require_text 'run the flagged ExternalAuth datapath test (#2284, option 2)' 'the operator must be pointed at the datapath test'
refute_text 'FAULT-PERSISTS' 'plaintext node keys with node encryption off must never report the fault'
assert_safe
pass 'node encryption off (key absent, the production shape) + all zero is INCONCLUSIVE and points to the datapath test'

reset_fixtures
edit_json cilium-config.json '.data["encrypt-node"] = "false"'
run_script --context admin@prod
require_rc 3 'node encryption off (explicit false) + all zero'
require_text 'Cilium node encryption: off' 'an explicit false must read as off'
refute_text 'FAULT-PERSISTS' 'explicit false must never report the fault'
assert_safe
pass 'node encryption off (explicit "false") + all zero is INCONCLUSIVE'

reset_fixtures
rm "${fixtures}/cilium-config.json"
run_script --context admin@prod
require_rc 3 'node encryption unreadable + all zero'
require_text 'Cilium node encryption: undetermined' 'an unreadable ConfigMap must read as undetermined'
require_text 'Cilium node encryption could not be determined' 'undetermined reason'
require_text 'run the flagged ExternalAuth datapath test (#2284, option 2)' 'undetermined must point to the datapath test'
refute_text 'FAULT-PERSISTS' 'an unreadable setting must never report the fault'
assert_safe
pass 'node encryption unreadable + all zero is INCONCLUSIVE'

reset_fixtures
edit_json cilium-config.json '.data["encrypt-node"] = "maybe"'
run_script --context admin@prod
require_rc 3 'node encryption unrecognised value + all zero'
require_text 'Cilium node encryption: undetermined' 'an unrecognised value must read as undetermined'
refute_text 'FAULT-PERSISTS' 'an unrecognised value must never report the fault'
assert_safe
pass 'node encryption with an unrecognised value + all zero is INCONCLUSIVE'

reset_fixtures
edit_json cilium-config.json 'del(.data["encrypt-node"])'
set_ipcache_key 255 10.0.0.3 10.0.0.4 10.0.0.5
run_script --context admin@prod
require_rc 0 'node encryption off + non-zero'
require_text 'VERDICT: PLAUSIBLY-FIXED' 'non-zero node keys keep PLAUSIBLY-FIXED whatever the setting'
assert_safe
pass 'node encryption off + non-zero node-address keys still reports PLAUSIBLY-FIXED'

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
require_no_exec 'must not exec in an agent on a replica node'
assert_safe
pass 'agents only on replica nodes: INCONCLUSIVE with no exec'

reset_fixtures
edit_json cilium-pods.json '.items |= map(if .spec.nodeName == "prod-control-plane-1" then .status.containerStatuses[0].ready = false else . end)'
# Seen from prod-worker-3, its own address is `host` and the control plane is a remote node.
set_ipcache_identity 1 10.0.0.5
set_ipcache_identity 6 10.0.0.2
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
require_no_exec 'must not exec while a replica is unsettled'
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

# --- Coverage: every remote node must be observed before any verdict ----------------------------

reset_fixtures
set_ipcache_key 255 10.0.0.3 10.0.0.4 10.0.0.5
drop_ipcache_entry 10.0.0.5
run_script --context admin@prod
require_rc 3 'uncovered remote node under the fixed shape'
require_text 'has no identity=6 entry for any of its addresses' 'uncovered reason'
refute_text 'PLAUSIBLY-FIXED' 'an unobserved source must not be reported as fixed'
assert_safe
pass 'a remote node with no entry is INCONCLUSIVE even when every observed entry is non-zero'

reset_fixtures
drop_ipcache_entry 10.0.0.4
run_script --context admin@prod
require_rc 3 'uncovered remote node under the fault shape'
require_text 'has no identity=6 entry for any of its addresses' 'uncovered reason (fault shape)'
refute_text 'FAULT-PERSISTS' 'an unobserved source must not be reported as the fault either'
assert_safe
pass 'a remote node with no entry is INCONCLUSIVE under the fault shape too'

# The autoscaler case from review: a node in the list whose ipcache entry has not propagated.
reset_fixtures
set_ipcache_key 255 10.0.0.3 10.0.0.4 10.0.0.5
edit_json nodes.json '.items += [{"metadata":{"name":"prod-worker-4","uid":"uid-node-w4"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.6"}]}}]'
run_script --context admin@prod
require_rc 3 'freshly added node'
require_text 'remote nodes covered: 3 of 4' 'coverage count must include the new node'
refute_text 'PLAUSIBLY-FIXED' 'a freshly added node must block the fixed verdict'
assert_safe
pass 'a freshly added node with no propagated entry is INCONCLUSIVE'

# Negative control for coverage: a node observed only through its ExternalIP IS covered, and the
# probed node (whose own address is `host`, identity=1) is never expected.
reset_fixtures
set_ipcache_key 255 10.0.0.3 10.0.0.4
awk '$1 == "10.0.0.5/32" { print "203.0.113.5/32      identity=6 encryptkey=255 tunnelendpoint=0.0.0.0 flags=<none>"; next } { print }' \
  "${fixtures}/ipcache.txt" >"${fixtures}/ipcache.tmp"
mv "${fixtures}/ipcache.tmp" "${fixtures}/ipcache.txt"
run_script --context admin@prod
require_rc 0 'ExternalIP coverage control'
require_text 'VERDICT: PLAUSIBLY-FIXED' 'ExternalIP coverage must count'
require_text 'remote nodes covered: 3 of 3' 'the probed node must not be expected'
assert_safe
pass 'negative control: ExternalIP-only coverage counts and the probed node is never expected'

reset_fixtures
edit_json nodes.json '.items[3].status.addresses = [{"type":"Hostname","address":"prod-worker-3"}]'
run_script --context admin@prod
require_rc 3 'node with no host address'
require_text 'reported no InternalIP or ExternalIP address' 'addressless node reason'
assert_safe
pass 'a node with no InternalIP or ExternalIP is INCONCLUSIVE'

# --- Replica stability: the Ready oauth2-proxy replicas must not change during the read ----------

reset_fixtures
replicas_after '.items[0].metadata.uid = "uid-pod-a-replacement"'
run_script --context admin@prod
require_rc 3 'replica replaced mid-read'
require_text 'the oauth2-proxy replicas changed during the read' 'replaced replica reason'
refute_text 'FAULT-PERSISTS' 'a replaced replica must block the verdict'
[[ "$(grep -c ' exec ' "${fixtures}/calls.log")" -eq 1 ]] || fail 'the read itself must still have happened exactly once'
assert_safe
pass 'a replica replaced with a new UID during the read is INCONCLUSIVE'

reset_fixtures
replicas_after '.items[1].spec.nodeName = "prod-worker-3"'
run_script --context admin@prod
require_rc 3 'replica moved mid-read'
require_text 'the oauth2-proxy replicas changed during the read' 'moved replica reason'
assert_safe
pass 'a replica moved to another node during the read is INCONCLUSIVE'

reset_fixtures
replicas_after '.items[0].status.podIP = "10.244.22.240" | .items[0].status.podIPs = [{"ip":"10.244.22.240"}]'
run_script --context admin@prod
require_rc 3 'replica pod IP changed mid-read'
require_text 'the oauth2-proxy replicas changed during the read' 'changed pod IP reason'
assert_safe
pass 'a replica whose pod IP changed during the read is INCONCLUSIVE'

reset_fixtures
replicas_after '.items[1].status.conditions[0].status = "False"'
run_script --context admin@prod
require_rc 3 'replica lost readiness mid-read'
require_text 'an oauth2-proxy pod is not settled after the read' 'unready replica reason'
assert_safe
pass 'a replica that stopped being Ready during the read is INCONCLUSIVE'

# The review case: a surge pod created between the first list and the exec that stays Pending. The
# old Ready set is unchanged, so only a check over EVERY selected pod can see it.
reset_fixtures
replicas_after '.items += [{"metadata":{"name":"oauth2-proxy-7c9d-ccccc","uid":"uid-pod-c"},"spec":{"nodeName":"prod-worker-3"},"status":{"phase":"Pending","conditions":[{"type":"Ready","status":"False"}]}}]'
run_script --context admin@prod
require_rc 3 'Pending surge pod mid-read'
require_text 'an oauth2-proxy pod is not settled after the read' 'Pending surge reason'
refute_text 'FAULT-PERSISTS' 'a Pending surge pod must block the verdict'
[[ "$(grep -c ' exec ' "${fixtures}/calls.log")" -eq 1 ]] || fail 'the read itself must still have happened exactly once'
assert_safe
pass 'a Pending surge pod that appears during the read is INCONCLUSIVE'

reset_fixtures
replicas_after '.items += [{"metadata":{"name":"oauth2-proxy-7c9d-ccccc","uid":"uid-pod-c"},"spec":{},"status":{"phase":"Pending"}}]'
run_script --context admin@prod
require_rc 3 'unscheduled surge pod mid-read'
require_text 'an oauth2-proxy pod is not settled after the read' 'unscheduled surge reason'
assert_safe
pass 'an unscheduled Pending surge pod (no node yet) is INCONCLUSIVE'

reset_fixtures
replicas_after '.items += [{"metadata":{"name":"oauth2-proxy-7c9d-ddddd","uid":"uid-pod-d"},"spec":{"nodeName":"prod-worker-3"},"status":{"phase":"Running","podIP":"10.244.25.7","podIPs":[{"ip":"10.244.25.7"}],"conditions":[{"type":"Ready","status":"False"}]}}]'
run_script --context admin@prod
require_rc 3 'Running-but-unready replacement mid-read'
require_text 'an oauth2-proxy pod is not settled after the read' 'unready replacement reason'
refute_text 'FAULT-PERSISTS' 'an unready replacement must block the verdict'
assert_safe
pass 'a Running-but-unready replacement that appears during the read is INCONCLUSIVE'

reset_fixtures
replicas_after '.items[0].metadata.deletionTimestamp = "2026-09-13T12:00:00Z"'
run_script --context admin@prod
require_rc 3 'terminating replica mid-read'
require_text 'an oauth2-proxy pod is not settled after the read' 'terminating replica reason'
assert_safe
pass 'a replica that started terminating during the read is INCONCLUSIVE'

# A new pod on the probed node breaks the no-local-replica precondition even when it is settled.
reset_fixtures
replicas_after '.items += [{"metadata":{"name":"oauth2-proxy-7c9d-eeeee","uid":"uid-pod-e"},"spec":{"nodeName":"prod-control-plane-1"},"status":{"phase":"Running","podIP":"10.244.24.9","podIPs":[{"ip":"10.244.24.9"}],"conditions":[{"type":"Ready","status":"True"}]}}]'
run_script --context admin@prod
require_rc 3 'new pod on the selected node'
require_text 'an oauth2-proxy pod is on the selected Cilium node after the read' 'local pod reason'
refute_text 'FAULT-PERSISTS' 'a pod on the selected node must block the verdict'
assert_safe
pass 'a new Ready pod on the selected Cilium node is INCONCLUSIVE'

reset_fixtures
replicas_after '.items += [{"metadata":{"name":"oauth2-proxy-7c9d-eeeee","uid":"uid-pod-e"},"spec":{"nodeName":"prod-control-plane-1"},"status":{"phase":"Pending"}}]'
run_script --context admin@prod
require_rc 3 'Pending pod on the selected node'
require_text 'an oauth2-proxy pod is on the selected Cilium node after the read' 'local Pending pod reason'
assert_safe
pass 'a Pending pod on the selected Cilium node is INCONCLUSIVE, whatever its state'

# Negative control: an unchanged settled set, re-read verbatim, still yields its verdict.
reset_fixtures
cp "${fixtures}/oauth2-pods.json" "${fixtures}/oauth2-pods-after.json"
run_script --context admin@prod
require_rc 0 'unchanged settled set control'
require_text 'VERDICT: FAULT-PERSISTS' 'an unchanged settled set must still yield its verdict'
require_text 'oauth2-proxy replicas: unchanged across the read' 'an unchanged set must read as unchanged'
assert_safe
pass 'negative control: an unchanged settled replica set still yields its verdict'

reset_fixtures
touch "${fixtures}/oauth2-pods-after-fails"
run_script --context admin@prod
require_rc 3 'replica re-read failure'
require_text 'could not re-list the oauth2-proxy pods after the read' 'replica re-read failure reason'
assert_safe
pass 'a failed oauth2-proxy re-read is INCONCLUSIVE'

reset_fixtures
edit_json oauth2-pods.json '.items[1].metadata.uid = ""'
run_script --context admin@prod
require_rc 3 'replica without UID'
require_text 'an oauth2-proxy replica has no UID, node or pod IP yet' 'missing replica UID reason'
require_no_exec 'must not exec when a replica identity is unknown'
assert_safe
pass 'a replica with no UID is INCONCLUSIVE before any exec'

# Negative control: identical replicas listed in another order, with the pod IP given only through
# podIPs, are unchanged and still yield the verdict.
reset_fixtures
replicas_after '.items |= (reverse | map(del(.status.podIP)))'
run_script --context admin@prod
require_rc 0 'reordered replicas control'
require_text 'VERDICT: FAULT-PERSISTS' 'identical replicas in another order must still yield the verdict'
require_text 'oauth2-proxy replicas: unchanged across the read' 'reordered replicas must read as unchanged'
assert_safe
pass 'negative control: identical replicas listed in another order still yield their verdict'

# --- Topology stability: the node set must not change during the read --------------------------

reset_fixtures
set_ipcache_key 255 10.0.0.3 10.0.0.4 10.0.0.5
nodes_after '.items += [{"metadata":{"name":"prod-worker-4","uid":"uid-node-w4"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.6"}]}}]'
run_script --context admin@prod
require_rc 3 'node added mid-read'
require_text 'the node set or a node address changed during the read' 'node added reason'
refute_text 'PLAUSIBLY-FIXED' 'a node added mid-read must block the fixed verdict'
[[ "$(grep -c ' exec ' "${fixtures}/calls.log")" -eq 1 ]] || fail 'the read itself must still have happened exactly once'
assert_safe
pass 'a node added between the node list and the exec is INCONCLUSIVE'

reset_fixtures
nodes_after '.items |= map(select(.metadata.name != "prod-worker-3"))'
run_script --context admin@prod
require_rc 3 'node removed mid-read'
require_text 'the node set or a node address changed during the read' 'node removed reason'
assert_safe
pass 'a node removed during the read is INCONCLUSIVE'

reset_fixtures
nodes_after '.items |= map(if .metadata.name == "prod-worker-2" then .status.addresses[0].address = "10.0.0.9" else . end)'
run_script --context admin@prod
require_rc 3 'address changed mid-read'
require_text 'the node set or a node address changed during the read' 'address changed reason'
assert_safe
pass 'a node address changed during the read is INCONCLUSIVE'

reset_fixtures
nodes_after '.items |= map(if .metadata.name == "prod-worker-3" then .metadata.uid = "uid-node-w3-replacement" else . end)'
run_script --context admin@prod
require_rc 3 'node replaced mid-read'
require_text 'the node set or a node address changed during the read' 'node replaced reason'
assert_safe
pass 'a node replaced under the same name (new UID) is INCONCLUSIVE'

# Negative control: the same topology returned in a different order is unchanged, and non-host
# address types (Hostname) do not count.
reset_fixtures
nodes_after '.items |= (reverse | map(.status.addresses |= (reverse + [{"type":"Hostname","address":"renamed-host"}])))'
run_script --context admin@prod
require_rc 0 'reordered topology control'
require_text 'VERDICT: FAULT-PERSISTS' 'an unchanged topology in another order must still yield its verdict'
require_text 'node topology: unchanged across the read' 'reordered topology must read as unchanged'
assert_safe
pass 'negative control: an unchanged node set in another order still yields its verdict'

reset_fixtures
touch "${fixtures}/nodes-after-fails"
run_script --context admin@prod
require_rc 3 'node re-list failure'
require_text 'could not re-list the nodes after the read' 'node re-list failure reason'
assert_safe
pass 'a failed node re-list is INCONCLUSIVE'

reset_fixtures
edit_json nodes.json '.items[2].metadata.uid = ""'
run_script --context admin@prod
require_rc 3 'node without UID'
require_text 'a node reported no name or UID' 'missing UID reason'
require_no_exec 'must not exec when node identity is unknown'
assert_safe
pass 'a node with no UID is INCONCLUSIVE before any exec'

# --- Rollout: the agent set must be fully rolled and the selected agent current -----------------

reset_fixtures
edit_json daemonset.json '.status.updatedNumberScheduled = 3'
run_script --context admin@prod
require_rc 3 'partially updated DaemonSet'
require_text 'partially rolled out (not every node runs the updated agent)' 'partial update reason'
require_no_exec 'must not exec while the DaemonSet is partially updated'
assert_safe
pass 'a partially updated Cilium DaemonSet is INCONCLUSIVE before any exec'

reset_fixtures
edit_json daemonset.json '.status.numberAvailable = 3'
run_script --context admin@prod
require_rc 3 'unavailable agent'
require_text 'partially rolled out (not every agent is available)' 'unavailable reason'
require_no_exec 'must not exec while an agent is unavailable'
assert_safe
pass 'a Cilium DaemonSet with an unavailable agent is INCONCLUSIVE before any exec'

reset_fixtures
edit_json daemonset.json '.status.observedGeneration = 6'
run_script --context admin@prod
require_rc 3 'generation not observed'
require_text 'has not observed its current generation' 'generation reason'
require_no_exec 'must not exec before the DaemonSet observes its generation'
assert_safe
pass 'a Cilium DaemonSet that has not observed its generation is INCONCLUSIVE before any exec'

reset_fixtures
# Only the SELECTED agent is left behind; the DaemonSet counters still read fully rolled, so this
# isolates the per-pod revision check from the counter checks above.
jq --arg old "${old_hash}" \
  '.items |= map(if .spec.nodeName == "prod-control-plane-1" then .metadata.labels["controller-revision-hash"] = $old else . end)' \
  "${fixtures}/cilium-pods.json" >"${fixtures}/cilium-pods.tmp"
mv "${fixtures}/cilium-pods.tmp" "${fixtures}/cilium-pods.json"
[[ "$(jq -r '.items[] | select(.spec.nodeName == "prod-control-plane-1") | .metadata.labels["controller-revision-hash"]' "${fixtures}/cilium-pods.json")" == "${old_hash}" ]] ||
  fail 'fixture: the selected pod must carry the old revision'
run_script --context admin@prod
require_rc 3 'selected pod on an old revision'
require_text 'the selected Cilium agent is not on the DaemonSet current revision' 'old revision reason'
require_no_exec 'must not exec in an agent on an old revision'
assert_safe
pass 'a selected agent on an old revision is INCONCLUSIVE before any exec'

reset_fixtures
edit_json cilium-pods.json '.items |= map(del(.metadata.labels["controller-revision-hash"]))'
run_script --context admin@prod
require_rc 3 'selected pod without revision'
require_text 'the selected Cilium agent is not on the DaemonSet current revision' 'missing pod revision reason'
require_no_exec 'must not exec in an agent whose revision is unknown'
assert_safe
pass 'a selected agent with no revision label is INCONCLUSIVE before any exec'

reset_fixtures
edit_json controllerrevisions.json '.items |= map(select(.metadata.ownerReferences[0].uid != "uid-ds-cilium"))'
run_script --context admin@prod
require_rc 3 'no owned revision'
require_text 'current revision could not be resolved' 'unresolved revision reason'
require_no_exec 'must not exec when the current revision is unknown'
assert_safe
pass 'no ControllerRevision owned by the DaemonSet is INCONCLUSIVE before any exec'

reset_fixtures
rm "${fixtures}/daemonset.json"
run_script --context admin@prod
require_rc 3 'DaemonSet read failure'
require_text 'could not read the Cilium DaemonSet' 'DaemonSet read failure reason'
require_no_exec 'must not exec when the DaemonSet cannot be read'
assert_safe
pass 'a failed DaemonSet read is INCONCLUSIVE before any exec'

# Negative control: a fully rolled DaemonSet whose current revision is NOT the last listed, next to a
# foreign DaemonSet's higher revision, still yields its verdict.
reset_fixtures
edit_json controllerrevisions.json '.items |= reverse'
set_ipcache_key 255 10.0.0.3 10.0.0.4 10.0.0.5
run_script --context admin@prod
require_rc 0 'fully rolled control'
require_text 'VERDICT: PLAUSIBLY-FIXED' 'a fully rolled DaemonSet must still yield its verdict'
require_text 'Cilium DaemonSet: fully rolled out; selected agent is on the current revision' 'rollout control text'
assert_safe
pass 'negative control: a fully rolled DaemonSet passes regardless of revision order and foreign revisions'

# --- Workflow contract ---------------------------------------------------------------------------
# The script's safety depends on how it is dispatched, so the workflow's shape is pinned here too:
# a dropped guard would otherwise be silent until a misleading or unreviewed run happened.

readonly workflow="${root_dir}/.github/workflows/diagnose-cilium-ext-authz-ipcache.yaml"
wf_fail() {
  printf 'FAIL: workflow contract: %s\n' "$1" >&2
  exit 1
}
wf_line() {
  # A missing line must reach the NAMED assertion, not abort the test silently: under pipefail a
  # grep with no match would fail the assignment and exit before any message is printed.
  { grep -n -F -- "$1" "${workflow}" || true; } | head -n 1 | cut -d: -f1
}
require_before() {
  local first="$1" second="$2" description="$3"
  [[ -n "${first}" && -n "${second}" && "${first}" -lt "${second}" ]] || wf_fail "${description}"
}

grep -Eq '^  group: prod-deploy$' "${workflow}" || wf_fail 'concurrency must serialise on prod-deploy'
grep -Eq '^  cancel-in-progress: false$' "${workflow}" || wf_fail 'cancel-in-progress must be false'
[[ "$(grep -Ec '^[[:space:]]*group:' "${workflow}")" -eq 1 ]] || wf_fail 'exactly one concurrency group'
pass 'workflow serialises on the prod-deploy lock'

guard_line="$(wf_line "if [[ \"\${RUN_REF}\" != 'refs/heads/main' ]]; then")"
# The `${{ … }}` strings below are GitHub expressions matched literally; nothing should expand.
# shellcheck disable=SC2016
ref_env_line="$(wf_line 'RUN_REF: ${{ github.ref }}')"
checkout_line="$(wf_line 'uses: actions/checkout@')"
restore_line="$(wf_line '🔑 Restore kubeconfig')"
[[ -n "${ref_env_line}" ]] || wf_fail 'the main-branch guard must read github.ref through env'
require_before "${guard_line}" "${checkout_line}" 'the main-branch guard must run before checkout'
require_before "${checkout_line}" "${restore_line}" 'checkout must precede the kubeconfig restore'
# shellcheck disable=SC2016
awk -v start="${checkout_line}" '
  NR > start && NR <= start + 6 && /^ +ref: \$\{\{ github\.sha \}\}$/ { found = 1 }
  END { exit !found }
' "${workflow}" || wf_fail 'checkout must be pinned to the dispatch commit (ref: ${{ github.sha }})'
pass 'workflow refuses any ref but main before checkout, and pins checkout'

endpoint_line="$(wf_line 'run: ./scripts/use-prod-stable-api-endpoint.sh')"
diagnose_line="$(wf_line 'run: ./scripts/diagnose-cilium-ext-authz-ipcache.sh --context admin@prod')"
# shellcheck disable=SC2016
hcloud_line="$(wf_line 'HCLOUD_TOKEN: ${{ secrets.HCLOUD_TOKEN }}')"
require_before "${restore_line}" "${endpoint_line}" 'endpoint selection must follow the kubeconfig restore'
require_before "${endpoint_line}" "${diagnose_line}" 'endpoint selection must precede the diagnostic'
require_before "${restore_line}" "${hcloud_line}" 'HCLOUD_TOKEN must be scoped to the endpoint step'
require_before "${hcloud_line}" "${endpoint_line}" 'HCLOUD_TOKEN must be scoped to the endpoint step'
# Count the secret REFERENCE, not the name: the header comments legitimately mention the name.
[[ "$(grep -c 'secrets\.HCLOUD_TOKEN' "${workflow}")" -eq 1 ]] || wf_fail 'HCLOUD_TOKEN may be referenced only in the endpoint step'
pass 'workflow selects the stable API endpoint before the read, with HCLOUD_TOKEN scoped to that step'

if grep -Eq '^[[:space:]]+(schedule|push|pull_request|pull_request_target|merge_group):' "${workflow}"; then
  wf_fail 'the diagnostic must stay dispatch-only'
fi
pass 'workflow stays dispatch-only'

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
