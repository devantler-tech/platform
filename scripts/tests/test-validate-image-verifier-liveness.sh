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

cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/kubectl-args"
cat "${FIXTURES}/nodes.json"
FAKE
chmod +x "${fake_bin}/kubectl"

readonly rules_type='imageverificationrules.security.talos.dev'
readonly roots_type='tuftrustedroots.security.talos.dev'
readonly rules_owner='security.ImageVerificationConfigController'
readonly roots_owner='security.TUFTrustedRootController'
readonly ksail_pattern='ghcr.io/devantler-tech/ksail*'
readonly provider_pattern='ghcr.io/devantler-tech/provider-upjet-*'
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
    resource_obj 0002 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' "${app_pattern}"
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
require_text "${output}" '3 rule(s) in phase running' 'case 1: counts every declared running rule'
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
  resource_obj 0002 running "${rules_owner}" 'ImageVerificationRules.security.talos.dev' 'ghcr.io/devantler-tech/stale-*'
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
 {"metadata":{"name":"worker-1"},"status":{"addresses":[{"type":"Hostname","address":"worker-1"},{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-2"},"status":{"addresses":[{"type":"Hostname","address":"worker-2"},{"type":"InternalIP","address":"10.0.1.5"}]}}
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
{"items":[{"metadata":{"name":"worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}}]}
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
 {"metadata":{"name":"worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-9"},"status":{"addresses":[{"type":"Hostname","address":"worker-9"}]}}
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
 {"metadata":{"name":"worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}}
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
 {"metadata":{"name":"worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.4"}]}},
 {"metadata":{"name":"worker-2"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.6"}]}}
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
printf 'all cases passed\n'
