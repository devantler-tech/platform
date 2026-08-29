#!/usr/bin/env bash
# Pin the behaviour of scripts/validate-image-verifier-liveness.sh.
#
# WHY THIS EXISTS. The script's whole job is to notice a condition that looks
# identical to health from every other angle: ImageVerificationConfig rules
# declared in the repository, rendered manifests matching, CI green — and
# nothing on the node actually enforcing them (devantler-tech/platform#2856).
# A regression here is silent by construction: the script would exit 0 and the
# cluster would read as enforcing. So these cases pin the FAILING paths, not
# just the happy one.
#
# The cases below pin BOTH directions of every verdict. A checker that can only
# fail is as useless as one that can only pass — and this script has been each
# of those in turn: until 2026-08-24 it asserted a containerd `bin_dir` that
# does not exist on this platform, so it reported a confident FAIL on nodes that
# were verifying correctly (#3108). Every RED case here therefore has a GREEN
# control that differs in exactly one fixture.
#
# talosctl and kubectl are faked from fixture directories; no cluster, no
# secrets, no network. Bash 3.2 compatible so it runs on a maintainer's macOS
# as well as CI.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/validate-image-verifier-liveness.sh"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

readonly fake_bin="${work_dir}/bin"
readonly fixtures="${work_dir}/fixtures"
mkdir -p "${fake_bin}" "${fixtures}"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

require_text() {
  local haystack="$1" needle="$2" description="$3"
  if ! grep -Fq -- "${needle}" <<<"${haystack}"; then
    printf 'FAIL: %s\n--- actual output ---\n%s\n---\n' "${description}" "${haystack}" >&2
    exit 1
  fi
}

refute_text() {
  local haystack="$1" needle="$2" description="$3"
  if grep -Fq -- "${needle}" <<<"${haystack}"; then
    printf 'FAIL: %s\n--- actual output ---\n%s\n---\n' "${description}" "${haystack}" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Fake talosctl: serves `get <type> -o json` from a fixture tree, and `ls /` as
# the reachability probe.
#
# Three behaviours mirror the real tool because the script depends on each:
#   * a type with NO instances exits 0 and emits NOTHING — that is a verdict
#     ("this node holds no rules"), not an error, and the script must tell it
#     apart from a failed query;
#   * a query that FAILS exits non-zero — an infrastructure fault, never a
#     verdict;
#   * a node with no fixture directory is UNREACHABLE: every subcommand fails,
#     including `ls /`. That lets the unreachable case be tested for real
#     instead of via a special-case flag.
# ---------------------------------------------------------------------------
cat >"${fake_bin}/talosctl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
node=''
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -n) node="$2"; shift 2 ;;
    read)
      # The script must never read a node FILE: /etc/cri/conf.d/cri.toml carries
      # registry credentials and this output goes to CI logs. A sentinel FILE is
      # used rather than stderr because the script redirects stderr on its query
      # path, so a stream-based marker could be swallowed silently.
      touch "${FIXTURES}/NODE_FILE_WAS_READ"
      exit 1
      ;;
    get)
      type="$2"
      # An unreachable node fails everything.
      [[ -d "${FIXTURES}/${node}" ]] || { printf 'error connecting to %s\n' "${node}" >&2; exit 1; }
      # A query that faults on a REACHABLE node.
      if [[ -f "${FIXTURES}/${node}/queryfail_${type}" ]]; then
        printf 'rpc error: code = Unavailable desc = connection error\n' >&2
        exit 1
      fi
      resource="${FIXTURES}/${node}/resources/${type}"
      # No instances of this type: exit 0, emit nothing.
      [[ -f "${resource}" ]] || exit 0
      cat "${resource}"
      exit 0
      ;;
    ls)
      shift
      path="${1:-/}"
      [[ -d "${FIXTURES}/${node}" ]] || { printf 'error connecting to %s\n' "${node}" >&2; exit 1; }
      [[ -f "${FIXTURES}/${node}/unreachable" ]] && { printf 'error connecting to %s\n' "${node}" >&2; exit 1; }
      [[ "${path}" == '/' ]] && exit 0
      printf 'lstat %s: no such file or directory\n' "${path}" >&2
      exit 1
      ;;
    *) shift ;;
  esac
done
exit 1
FAKE
chmod +x "${fake_bin}/talosctl"

# ---------------------------------------------------------------------------
# Fake kubectl: serves the node inventory, and can serve a DIFFERENT one per
# call so a fleet that changes mid-run can be tested.
#
# `nodes.json.<n>` is served on the n-th call, `nodes.json.last` on any call
# past the end of that sequence, and plain `nodes.json` when no sequence is
# staged. A per-call inventory is the only way to exercise the convergence
# pass: a single static fixture cannot distinguish a check that re-reads the
# fleet from one that trusts its opening snapshot.
# ---------------------------------------------------------------------------
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/kubectl-args"
calls=$(( $(cat "${FIXTURES}/kubectl-calls" 2>/dev/null || printf '0') + 1 ))
printf '%s' "${calls}" >"${FIXTURES}/kubectl-calls"
if [[ -f "${FIXTURES}/nodes.json.${calls}" ]]; then
  cat "${FIXTURES}/nodes.json.${calls}"
elif [[ -f "${FIXTURES}/nodes.json.last" ]]; then
  cat "${FIXTURES}/nodes.json.last"
else
  cat "${FIXTURES}/nodes.json"
fi
FAKE
chmod +x "${fake_bin}/kubectl"

# Every staged inventory and the call counter, cleared between cases so one
# case's sequence cannot leak into the next and silently change what it tests.
reset_inventory() {
  rm -f "${fixtures}"/nodes.json.* "${fixtures}/kubectl-calls"
}

readonly rules_type='imageverificationrules.security.talos.dev'
readonly roots_type='tuftrustedroots.security.talos.dev'
readonly rules_owner='security.ImageVerificationConfigController'
readonly roots_owner='security.TUFTrustedRootController'
readonly ksail_pattern='ghcr.io/devantler-tech/ksail*'
readonly provider_pattern='ghcr.io/devantler-tech/provider-upjet-*'
readonly storage_pattern='ghcr.io/devantler-tech/platform-kubescape-storage'
readonly app_pattern='ghcr.io/devantler-tech/*'

write_node() {
  mkdir -p "${fixtures}/$1/resources"
}

# Emits one resource object in the shape `talosctl get -o json` produces: a
# STREAM of objects, not an array.
resource_obj() {
  local id="$1" phase="$2" owner="$3" type="$4" image_pattern="${5:-}"
  local spec='{}'
  if [[ -n "${image_pattern}" ]]; then
    spec="{\"imagePattern\":\"${image_pattern}\"}"
  fi
  cat <<EOF
{
    "metadata": {
        "id": "${id}",
        "namespace": "security",
        "owner": "${owner}",
        "phase": "${phase}",
        "type": "${type}",
        "version": 1
    },
    "node": "fixture",
    "spec": ${spec}
}
EOF
}

write_rules() {
  cat >"${fixtures}/$1/resources/${rules_type}"
}

write_roots() {
  cat >"${fixtures}/$1/resources/${roots_type}"
}

# The complete ordered rule set declared by the manifest.
write_healthy_rules() {
  local node="$1"
  {
    resource_obj 0000 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${ksail_pattern}"
    resource_obj 0001 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${provider_pattern}"
    resource_obj 0002 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${storage_pattern}"
    resource_obj 0003 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${app_pattern}"
  } | write_rules "${node}"
}

# The healthy shape, used as the control for every RED case below.
healthy_node() {
  local node="$1"
  write_node "${node}"
  write_healthy_rules "${node}"
  resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
    write_roots "${node}"
}

run_script() {
  env PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" \
    TALOSCTL="${fake_bin}/talosctl" KUBECTL="${fake_bin}/kubectl" \
    "$@" bash "${script}"
}

# ===========================================================================
# Case 1 — GREEN: rules in phase running, trust root in phase running.
# ===========================================================================
healthy_node good
output="$(run_script TALOS_NODES=good 2>&1)" || fail "case 1: expected exit 0 for a node that can enforce"
require_text "${output}" 'OK   good' 'case 1: reports the healthy node'
require_text "${output}" '4 rule(s) in phase running' 'case 1: counts every declared running rule'
require_text "${output}" 'All 1 node(s) can enforce image verification.' 'case 1: reports the summary'

# ===========================================================================
# Case 1a — RED: SOME declared rules materialised and are running, but the
# catch-all rule is absent. A presence-only check reports this node healthy even
# though images matched only by that rule are pulled without verification.
# ===========================================================================
write_node partialrules
{
  resource_obj 0000 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${ksail_pattern}"
  resource_obj 0001 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${provider_pattern}"
} | write_rules partialrules
resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots partialrules
status=0
output="$(run_script TALOS_NODES=partialrules 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 1a: a node missing one declared ImageVerificationRules resource MUST fail'
require_text "${output}" 'FAIL partialrules' 'case 1a: names the partially materialised node'
require_text "${output}" 'declared rule set' 'case 1a: names the incomplete policy'

# ===========================================================================
# Case 1b — RED: the right NUMBER of rules is running, but the last pattern is
# stale. Counts alone cannot prove that the declared ordered policy materialised.
# ===========================================================================
write_node driftrules
{
  resource_obj 0000 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${ksail_pattern}"
  resource_obj 0001 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${provider_pattern}"
  resource_obj 0002 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${storage_pattern}"
  resource_obj 0003 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' 'ghcr.io/devantler-tech/stale-*'
} | write_rules driftrules
resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots driftrules
status=0
output="$(run_script TALOS_NODES=driftrules 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 1b: a runtime rule pattern that differs from the declaration MUST fail'
require_text "${output}" 'FAIL driftrules' 'case 1b: names the node with policy drift'
require_text "${output}" 'declared rule set' 'case 1b: names the drifted policy'

# ===========================================================================
# Case 2 — RED: the node holds NO ImageVerificationRules at all. The declared
# rules were never materialised, so nothing constrains a pull.
# ===========================================================================
write_node norules
resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots norules
status=0
output="$(run_script TALOS_NODES=norules 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 2: a node with no ImageVerificationRules MUST fail'
require_text "${output}" 'FAIL norules' 'case 2: names the failing node'
require_text "${output}" 'never materialised' 'case 2: names the condition that failed'

# ===========================================================================
# Case 3 — RED: rules exist as resources but none reached phase "running".
# This is the case a presence-only check would pass.
# ===========================================================================
write_node notrunning
{
  resource_obj 0000 tearingDown "${rules_owner}" 'ImageVerificationRules.security.talos.dev'
} | write_rules notrunning
resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots notrunning
status=0
output="$(run_script TALOS_NODES=notrunning 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 3: rules present but not running MUST fail'
require_text "${output}" 'FAIL notrunning' 'case 3: names the failing node'
require_text "${output}" 'not being enforced' 'case 3: names the condition that failed'

# ===========================================================================
# Case 4 — RED: a rule in phase running but owned by something OTHER than the
# ImageVerificationConfig controller. Without the owner check, any resource of
# that type parked in the namespace would vouch for enforcement.
# ===========================================================================
write_node wrongowner
resource_obj 0000 running some.other.Controller 'ImageVerificationRules.security.talos.dev' |
  write_rules wrongowner
resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots wrongowner
status=0
output="$(run_script TALOS_NODES=wrongowner 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 4: a rule owned by another controller MUST NOT satisfy the check'
require_text "${output}" 'FAIL wrongowner' 'case 4: names the failing node'
require_text "${output}" 'not being enforced' 'case 4: names the condition that failed'

# ===========================================================================
# Case 5 — RED: running rules but NO trust root. Keyless verification has
# nothing to verify a signature against, so the rules decide nothing. This is a
# DIFFERENT failure from a missing rule and must be reported as its own.
# ===========================================================================
write_node noroot
write_healthy_rules noroot
status=0
output="$(run_script TALOS_NODES=noroot 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 5: running rules with no trust root MUST fail'
require_text "${output}" 'FAIL noroot' 'case 5: names the failing node'
require_text "${output}" 'no trust material' 'case 5: names the condition that failed'

# ===========================================================================
# Case 6 — RED: a trust root that exists but is not running.
# ===========================================================================
write_node staleroot
write_healthy_rules staleroot
resource_obj trusted_root.json tearingDown "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots staleroot
status=0
output="$(run_script TALOS_NODES=staleroot 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 6: a non-running trust root MUST fail'
require_text "${output}" 'FAIL staleroot' 'case 6: names the failing node'
require_text "${output}" 'no usable trust material' 'case 6: names the condition that failed'

# ===========================================================================
# Case 7 — a mixed fleet fails and reports EVERY bad node, not just the first.
# ===========================================================================
status=0
output="$(run_script TALOS_NODES=good,norules,noroot 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 7: a fleet containing an unenforcing node MUST fail'
require_text "${output}" 'OK   good' 'case 7: still reports the healthy node'
require_text "${output}" 'FAIL norules' 'case 7: reports the node with no rules'
require_text "${output}" 'FAIL noroot' 'case 7: reports the node with no trust root'
require_text "${output}" '2 of 3 node(s) cannot enforce' 'case 7: counts every failure, not just the first'

# ===========================================================================
# Case 8 — a query that FAULTS on a reachable node is an infrastructure error
# (exit 2), never a verdict. Reporting it as "cannot enforce" would blame the
# cluster for a runner-side fault; swallowing it would be a fail-open.
# ===========================================================================
healthy_node faulty
touch "${fixtures}/faulty/queryfail_${rules_type}"
status=0
output="$(run_script TALOS_NODES=faulty 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail 'case 8: a failed query MUST be an infrastructure error, not a verdict'
require_text "${output}" 'could not read' 'case 8: names the query it could not run'
refute_text "${output}" 'FAIL faulty' 'case 8: reported a verdict for a node it never inspected'
rm -f "${fixtures}/faulty/queryfail_${rules_type}"
output="$(run_script TALOS_NODES=faulty 2>&1)" || fail 'case 8: the same node must pass once the query fault is gone'
require_text "${output}" 'OK   faulty' 'case 8: control — the node is otherwise healthy'

# ===========================================================================
# Case 9 — a trust-root query that faults is likewise an infrastructure error,
# not a "no trust material" verdict.
# ===========================================================================
healthy_node rootfaulty
touch "${fixtures}/rootfaulty/queryfail_${roots_type}"
status=0
output="$(run_script TALOS_NODES=rootfaulty 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail 'case 9: a failed trust-root query MUST be an infrastructure error'
refute_text "${output}" 'no trust material' 'case 9: misreported a query fault as a missing trust root'

# ===========================================================================
# Case 10 — an UNREACHABLE node aborts; it is never reported as checked.
# ===========================================================================
status=0
output="$(run_script TALOS_NODES=ghost 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail 'case 10: an unreachable node MUST abort, not yield a verdict'
require_text "${output}" 'ghost' 'case 10: names the unreachable node'
refute_text "${output}" 'OK   ghost' 'case 10: reported a verdict for a node it never checked'

# ===========================================================================
# Case 11 — node discovery. With no --nodes/TALOS_NODES the script reads the
# fleet from kubectl and must use InternalIP, never Hostname.
# ===========================================================================
healthy_node 10.0.1.4
write_node 10.0.1.5
resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots 10.0.1.5
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"Hostname","address":"worker-1"},{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-2","uid":"uid-worker-2"},"status":{"addresses":[{"type":"Hostname","address":"worker-2"},{"type":"InternalIP","address":"10.0.1.5"}]}}
]}
EOF
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] || fail 'case 11: discovery must surface the unenforcing discovered node'
require_text "${output}" 'OK   10.0.1.4' 'case 11: discovered the healthy node'
require_text "${output}" 'FAIL 10.0.1.5' 'case 11: discovered the unenforcing node'
refute_text "${output}" 'worker-1' 'case 11: used InternalIP, not Hostname'

# ===========================================================================
# Case 12 — KUBECTL_CONTEXT is forwarded to kubectl. The restored kubeconfig's
# current-context is not guaranteed to be prod, so an unpinned context would
# enumerate the wrong fleet and still exit 0.
# ===========================================================================
rm -f "${fixtures}/kubectl-args"
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[{"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}}]}
EOF
run_script KUBECTL_CONTEXT=admin@prod >/dev/null 2>&1 || true
require_text "$(cat "${fixtures}/kubectl-args")" '--context admin@prod' 'case 12: KUBECTL_CONTEXT was not forwarded to kubectl'
rm -f "${fixtures}/kubectl-args"
run_script >/dev/null 2>&1 || true
refute_text "$(cat "${fixtures}/kubectl-args")" '--context' 'case 12: passed an empty --context when KUBECTL_CONTEXT was unset'

# ===========================================================================
# Case 13 — an empty entry in the node list is a USAGE error (exit 2), not a
# silently shortened fleet. Checking FEWER nodes than were asked for and still
# exiting 0 is a checker reporting a clean fleet it never looked at.
# ===========================================================================
for bad_list in ',good' 'good,' 'good,,good'; do
  status=0
  output="$(run_script TALOS_NODES="${bad_list}" 2>&1)" || status=$?
  [[ "${status}" -eq 2 ]] || fail "case 13: a malformed node list (${bad_list}) MUST be a usage error"
  require_text "${output}" 'empty entry' "case 13: names the malformation in ${bad_list}"
done
output="$(run_script TALOS_NODES='good,good' 2>&1)" || fail 'case 13: a well-formed comma-separated list must still be accepted'

# ===========================================================================
# Case 14 — an explicitly EMPTY node list is a usage error, not a fallback to
# cluster discovery: the option WAS supplied and named nothing, and discovery
# would check a different fleet than the caller asked for.
# ===========================================================================
status=0
output="$(run_script TALOS_NODES= 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail 'case 14: --nodes "" must not silently fall back to cluster discovery'
refute_text "${output}" 'OK   ' 'case 14: reported a verdict for a fleet the caller never asked for'

# ===========================================================================
# Case 15 — PARTIAL node discovery must abort, never pass on the prefix. A node
# with no InternalIP matches nothing and the jq select SUCCEEDS, so that node
# silently leaves the fleet while the rest report OK.
# ===========================================================================
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-9","uid":"uid-worker-9"},"status":{"addresses":[{"type":"Hostname","address":"worker-9"}]}}
]}
EOF
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail 'case 15: a node with no InternalIP MUST NOT yield exit 0 — it was never checked'
require_text "${output}" 'worker-9' 'case 15: names the node that has no InternalIP'
refute_text "${output}" 'All 1 node(s)' 'case 15: reported a verdict for a fleet that was only partly enumerated'

# ===========================================================================
# Case 16 — two nodes publishing the SAME InternalIP must not pass: both passes
# inspect the same machine while the other node is never looked at.
# ===========================================================================
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-2","uid":"uid-worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}}
]}
EOF
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail 'case 16: two nodes sharing one InternalIP MUST NOT yield exit 0'
require_text "${output}" 'worker-1 and worker-2' 'case 16: names the nodes sharing an address'

# Control: distinct addresses are still accepted, so case 16 is not passing for
# an unrelated reason.
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-2","uid":"uid-worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.6"}]}}
]}
EOF
healthy_node 10.0.1.6
output="$(run_script 2>&1)" || fail 'case 16: a fleet of distinct nodes with distinct InternalIPs must still be accepted'
require_text "${output}" 'OK   10.0.1.4' 'case 16: control reports the first discovered node'
require_text "${output}" 'OK   10.0.1.6' 'case 16: control reports the second discovered node'

# ===========================================================================
# Case 17 — SECURITY: the check must never read a node FILE. Until 2026-08-24
# it parsed /etc/cri/conf.d/cri.toml, which carries registry credentials on
# this cluster, and this script's output goes to CI logs. Removing that read
# removed the exposure; this pins it so a future change cannot quietly restore
# it. The fake records any `talosctl read` in a sentinel FILE, because the
# script redirects stderr on its query path and a stream marker could be
# swallowed.
# ===========================================================================
rm -f "${fixtures}/NODE_FILE_WAS_READ"
healthy_node credsafe
output="$(run_script TALOS_NODES=credsafe 2>&1)" || fail 'case 17: control — the healthy node must still pass'
require_text "${output}" 'OK   credsafe' 'case 17: control — reports the healthy node'
[[ ! -e "${fixtures}/NODE_FILE_WAS_READ" ]] ||
  fail 'case 17: the check read a node file — registry credentials are reachable from a script whose output goes to CI logs'

# ===========================================================================
# Case 18 — RED: a node that JOINS while the serial pass is running must not be
# skipped. The pass opens on a one-node fleet and a second, unenforcing worker
# is present by the time it finishes.
#
# Reading the opening snapshot only, the script inspects 10.0.2.1, finds it
# healthy and prints "All 1 node(s) can enforce image verification" — a green
# verdict for a fleet that now contains a node it never looked at. That is the
# same silence this whole script exists to break, one level up: not a node
# lying about its state, but a node nobody asked.
# ===========================================================================
reset_inventory
healthy_node 10.0.2.1
write_node 10.0.2.2
resource_obj trusted_root.json running "${roots_owner}" 'TUFTrustedRoots.security.talos.dev' |
  write_roots 10.0.2.2
cat >"${fixtures}/nodes.json.1" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.2.1"}]}}
]}
EOF
cat >"${fixtures}/nodes.json.last" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.2.1"}]}},
 {"metadata":{"name":"worker-2","uid":"uid-worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.2.2"}]}}
]}
EOF
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 1 ]] ||
  fail "case 18: a node that joined mid-pass was never inspected (expected exit 1, got ${status})"
require_text "${output}" 'FAIL 10.0.2.2' 'case 18: the node that joined mid-pass must be checked, not skipped'
refute_text "${output}" 'All 1 node(s) can enforce' 'case 18: reported a green verdict for a stale one-node snapshot'

# ===========================================================================
# Case 18a — GREEN control: the SAME two-node fleet, held still. Differs from
# case 18 in exactly one fixture — the opening inventory already lists both
# nodes — which proves case 18 is about the inventory changing and not merely
# about 10.0.2.2 being unhealthy.
# ===========================================================================
reset_inventory
healthy_node 10.0.2.3
healthy_node 10.0.2.4
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.2.3"}]}},
 {"metadata":{"name":"worker-2","uid":"uid-worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.2.4"}]}}
]}
EOF
output="$(run_script 2>&1)" || fail 'case 18a: control — a stable healthy fleet must pass'
require_text "${output}" 'All 2 node(s) can enforce image verification.' 'case 18a: control — reports both nodes'

# ===========================================================================
# Case 19 — RED: an autoscaler replacement that REUSES the departed node's
# InternalIP is a different node, and identity must say so.
#
# Every reading here lists exactly one node at 10.0.3.1; only the UID changes.
# Compared on address alone the fleet looks perfectly stable, so the script
# would converge immediately and report a node it never inspected as healthy.
# Compared on UID the churn is visible, and the run refuses rather than
# blessing one transient snapshot.
# ===========================================================================
reset_inventory
healthy_node 10.0.3.1
i=1
while [[ "${i}" -le 8 ]]; do
  cat >"${fixtures}/nodes.json.${i}" <<EOF
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-generation-${i}"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.3.1"}]}}
]}
EOF
  i=$((i + 1))
done
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] ||
  fail "case 19: a replacement reusing an address was mistaken for the original (expected exit 2, got ${status})"
require_text "${output}" 'will not hold still' 'case 19: must name the churning inventory as the reason it refuses'
refute_text "${output}" 'All 1 node(s) can enforce' 'case 19: reported a green verdict across a node replacement'

# ===========================================================================
# Case 19a — GREEN control: the same single node at the same address, with a
# STABLE UID. Differs from case 19 in exactly one fixture, so it proves the
# refusal above is caused by the identity changing rather than by re-reading
# the inventory at all — a check that refused every fleet would pass case 19
# while being useless.
# ===========================================================================
reset_inventory
healthy_node 10.0.3.2
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-stable"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.3.2"}]}}
]}
EOF
output="$(run_script 2>&1)" || fail 'case 19a: control — a stable node must pass'
require_text "${output}" 'All 1 node(s) can enforce image verification.' 'case 19a: control — reports the stable node'

# ===========================================================================
# Case 19b — a fleet that settles after ONE change is retried, not failed.
# Ordinary autoscaling changes the fleet occasionally, and a check that turned
# every such change into a hard failure would flap often enough to be ignored —
# which is indistinguishable from not having the check.
# ===========================================================================
reset_inventory
healthy_node 10.0.3.3
healthy_node 10.0.3.4
cat >"${fixtures}/nodes.json.1" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.3.3"}]}}
]}
EOF
cat >"${fixtures}/nodes.json.last" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.3.3"}]}},
 {"metadata":{"name":"worker-2","uid":"uid-worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.3.4"}]}}
]}
EOF
output="$(run_script 2>&1)" || fail 'case 19b: a fleet that settles after one change must be retried, not failed'
require_text "${output}" 'All 2 node(s) can enforce image verification.' 'case 19b: the settled fleet is what the verdict describes'
require_text "${output}" 're-checking (attempt 2 of 3)' 'case 19b: must say it re-checked rather than silently changing its answer'

# ===========================================================================
# Case 20 — RED: a node with no metadata.uid is refused, not silently given an
# empty identity. Two such nodes would compare equal to each other, collapsing
# identity back to the address exactly where it matters most. The API server
# always sets a UID, so its absence is a malformed inventory.
# ===========================================================================
reset_inventory
healthy_node 10.0.4.1
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.4.1"}]}}
]}
EOF
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail "case 20: a node with no UID must be refused (expected exit 2, got ${status})"
require_text "${output}" 'has no metadata.uid' 'case 20: must name the node missing its UID'

# ===========================================================================
# Case 20a — RED: two nodes sharing one UID is the same collapse from the other
# direction, and would make a replaced node compare equal to its predecessor.
# ===========================================================================
reset_inventory
healthy_node 10.0.4.2
healthy_node 10.0.4.3
cat >"${fixtures}/nodes.json" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-duplicate"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.4.2"}]}},
 {"metadata":{"name":"worker-2","uid":"uid-duplicate"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.4.3"}]}}
]}
EOF
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] || fail "case 20a: two nodes sharing a UID must be refused (expected exit 2, got ${status})"
require_text "${output}" 'share UID uid-duplicate' 'case 20a: must name the shared UID'

# ===========================================================================
# Case 21 — an explicitly pinned fleet is NOT converged on. --nodes is the
# caller's claim about what to check, not a snapshot of a moving cluster, so
# the convergence pass must not call kubectl at all — doing so would make a
# pinned run depend on cluster access it deliberately does not need.
# ===========================================================================
reset_inventory
rm -f "${fixtures}/kubectl-args"
healthy_node pinned
output="$(run_script TALOS_NODES=pinned 2>&1)" || fail 'case 21: a pinned fleet must still pass'
require_text "${output}" 'All 1 node(s) can enforce image verification.' 'case 21: reports the pinned node'
[[ ! -s "${fixtures}/kubectl-args" ]] ||
  fail 'case 21: a pinned run consulted kubectl — convergence must not apply to an explicitly named fleet'

# ===========================================================================
# Case 22 — RED: the fleet draining to EMPTY between readings must not be
# reported as a clean fleet.
#
# The "no nodes to check" guard runs once, before the sweep. The convergence
# pass re-reads the inventory afterwards, so a reading that comes back empty —
# a wrong context, an API server returning nothing, a cluster genuinely torn
# down — would otherwise settle (empty equals empty), sweep zero nodes, and
# print "All 0 node(s) can enforce image verification". Exit 0 over a fleet
# that does not exist is the most confident wrong answer this script can give.
# ===========================================================================
reset_inventory
healthy_node 10.0.5.1
cat >"${fixtures}/nodes.json.1" <<'EOF'
{"items":[
 {"metadata":{"name":"worker-1","uid":"uid-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.5.1"}]}}
]}
EOF
cat >"${fixtures}/nodes.json.last" <<'EOF'
{"items":[]}
EOF
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] ||
  fail "case 22: an empty inventory must be refused, not reported clean (expected exit 2, got ${status})"
require_text "${output}" 'no nodes to check' 'case 22: must name the empty inventory as the reason it refuses'
refute_text "${output}" 'All 0 node(s) can enforce' 'case 22: reported a green verdict for an empty fleet'
printf 'all cases passed\n'
