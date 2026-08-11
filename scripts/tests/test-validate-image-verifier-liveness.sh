#!/usr/bin/env bash
# Pin the behaviour of scripts/validate-image-verifier-liveness.sh.
#
# WHY THIS EXISTS. The script's whole job is to notice a condition that looks
# identical to health from every other angle: ImageVerificationConfig rules
# present and correct, node config matching the repository, CI green — and no
# verifier binary, so containerd permits every pull (devantler-tech/platform#2856).
# A regression here is silent by construction: the script would exit 0 and the
# cluster would read as enforcing. So these cases pin the FAILING paths, not just
# the happy one, and the #2856 state (bin_dir present, directory present, empty)
# gets its own case because it is the one that actually happened.
#
# The credential case is not decoration. The script reads
# /etc/cri/conf.d/cri.toml, which carries registry auth on this cluster, and its
# output goes to CI logs. A refactor that echoes the file — or quotes it in an
# error — would publish a token. That is pinned here so it fails loudly.
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
# Fake talosctl: serves `read <file>` and `ls [-l] <path>` from a fixture tree.
# A path with no fixture behaves like the real thing — non-zero, nothing on
# stdout — which is what makes the "directory does not exist" case real rather
# than simulated by a special-case flag.
#
# Two behaviours mirror the real tool because the script now depends on them:
#   * `ls` reports EXISTENCE for files as well as directories (verified against
#     prod: `talosctl ls /etc/cri/conf.d/cri.toml` succeeds), so it can probe a
#     config file without reading — and therefore without touching the registry
#     credentials that file carries.
#   * a node with no fixture directory at all is UNREACHABLE: every subcommand
#     fails, including `ls /`. That is what lets the unreachable case be tested
#     for real instead of via a special-case flag.
# ---------------------------------------------------------------------------
cat >"${fake_bin}/talosctl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
node=''
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -n) node="$2"; shift 2 ;;
    read)
      path="$2"
      file="${FIXTURES}/${node}/files/${path//\//_}"
      [[ -f "${file}" ]] || exit 1
      cat "${file}"
      exit 0
      ;;
    ls)
      shift
      long=0
      [[ "${1:-}" == '-l' ]] && { long=1; shift; } || true
      path="${1:-/}"
      # An unreachable node fails everything, exactly as the real client does
      # when it cannot talk to the API.
      [[ -d "${FIXTURES}/${node}" ]] || { printf 'error connecting to %s\n' "${node}" >&2; exit 1; }
      listing="${FIXTURES}/${node}/dirs/${path//\//_}"
      content="${FIXTURES}/${node}/files/${path//\//_}"
      # The node drops out mid-check: its config files still probe and read, but
      # every OTHER `ls` now fails — including the `/` reachability probe. That
      # ordering is the point: the check gets past the config and only then finds
      # the node gone, which is the path a start-of-run probe cannot cover.
      if [[ -f "${FIXTURES}/${node}/lsfail_all" && ! -f "${content}" ]]; then
        printf 'error connecting to %s\n' "${node}" >&2
        exit 1
      fi
      # A path that fails for a reason OTHER than being absent: the real client
      # emits an RPC error here, not an lstat one. Kept distinct from the
      # no-fixture case below so a transient fault can be told apart from a
      # config the node genuinely does not ship.
      if [[ -f "${FIXTURES}/${node}/lserror${path//\//_}" ]]; then
        printf 'rpc error: code = Unavailable desc = connection error\n' >&2
        exit 1
      fi
      # `ls /` is the reachability probe: the node answered, so it succeeds.
      [[ "${path}" == '/' ]] && exit 0
      if [[ -f "${listing}" ]]; then
        # A node that answers the short-form existence probe and then fails the
        # long-form listing: the directory was there, the node stopped talking.
        if [[ "${long}" -eq 1 && -f "${FIXTURES}/${node}/lsfail" ]]; then
          printf 'error connecting to %s\n' "${node}" >&2
          exit 1
        fi
        [[ "${long}" -eq 1 ]] && cat "${listing}"
        exit 0
      fi
      # A regular file exists but has no directory listing — `ls` on it succeeds
      # and never emits its CONTENTS.
      [[ -f "${content}" ]] && exit 0
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
cat "${FIXTURES}/nodes.json"
FAKE
chmod +x "${fake_bin}/kubectl"

# A real `talosctl ls -l` header plus rows. Column 2 is MODE, last column is
# NAME, and the LABEL column is EMPTY for some files on this cluster — which is
# why the script must not parse positionally from the left past column 2. One
# fixture below reproduces that empty-label row on purpose.
listing_header='NODE       MODE         UID   GID   SIZE(B)   LASTMOD           LABEL                                      NAME'

write_node() {
  local node="$1"
  mkdir -p "${fixtures}/${node}/files" "${fixtures}/${node}/dirs"
}

write_config() {
  local node="$1" path="$2"
  cat >"${fixtures}/${node}/files/${path//\//_}"
}

write_dir() {
  local node="$1" path="$2"
  cat >"${fixtures}/${node}/dirs/${path//\//_}"
}

run_script() {
  env PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" \
    TALOSCTL="${fake_bin}/talosctl" KUBECTL="${fake_bin}/kubectl" \
    "$@" bash "${script}"
}

# --- The credential value that must never appear in output. -----------------
# Deliberately NOT shaped like a real token. The property under test is "no
# config content reaches stdout", which any distinctive sentinel proves — while
# a realistic `ghp_`-shaped literal is itself a finding for the secret scanners
# this repository gates on (betterleaks/secretlint/trufflehog report 0 on this
# tree, and a test fixture is not a good reason to spend that).
readonly canary='CANARY-registry-auth-value-must-never-be-printed'

verifier_config() {
  local bin_dir="$1"
  cat <<EOF
version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.registry.configs.'ghcr.io'.auth]
      password = '${canary}'
      username = 'devantler'

  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '${bin_dir}'
EOF
}

no_verifier_config() {
  cat <<EOF
version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.registry.configs.'ghcr.io'.auth]
      password = '${canary}'
      username = 'devantler'
EOF
}

# ===========================================================================
# Case 1 — GREEN: bin_dir configured, exists, holds an executable.
# ===========================================================================
write_node good
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/good/files/_etc_cri_conf.d_cri.toml"
write_dir good /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   drwxr-xr-x   0     0     37        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   .
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

output="$(run_script TALOS_NODES=good 2>&1)" || fail "case 1: expected exit 0 for a node that can enforce"
require_text "${output}" 'OK   good' 'case 1: reports the healthy node'
require_text "${output}" 'All 1 node(s) can enforce image verification.' 'case 1: reports the summary'

# ===========================================================================
# Case 2 — RED, and this is the #2856 state: bin_dir configured, directory
# present, NO executable in it. containerd permits every pull here.
# ===========================================================================
write_node empty
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/empty/files/_etc_cri_conf.d_cri.toml"
write_dir empty /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   drwxr-xr-x   0     0     3         Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   .
EOF

if output="$(run_script TALOS_NODES=empty 2>&1)"; then
  fail "case 2: an empty bin_dir MUST fail — this is the exact state that let an unsigned image run (#2856)"
fi
require_text "${output}" 'FAIL empty' 'case 2: names the failing node'
require_text "${output}" 'holds no executable' 'case 2: names the condition that failed'

# ===========================================================================
# Case 3 — RED: no bin_dir configured at all in any config file. This is what
# the cluster actually looked like when #2856 was found.
# ===========================================================================
write_node unset
no_verifier_config >"${fixtures}/unset/files/_etc_cri_conf.d_cri.toml"
no_verifier_config >"${fixtures}/unset/files/_etc_containerd_config.toml"

if output="$(run_script TALOS_NODES=unset 2>&1)"; then
  fail 'case 3: a node with no bin_dir configured MUST fail'
fi
require_text "${output}" 'FAIL unset' 'case 3: names the failing node'
require_text "${output}" 'no bin_dir in the io.containerd.image-verifier.v1.bindir table' 'case 3: names the condition that failed'

# ===========================================================================
# Case 4 — RED: bin_dir configured but the directory does not exist.
# ===========================================================================
write_node missing
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/missing/files/_etc_cri_conf.d_cri.toml"

if output="$(run_script TALOS_NODES=missing 2>&1)"; then
  fail 'case 4: a configured-but-absent bin_dir MUST fail'
fi
require_text "${output}" 'does not exist' 'case 4: names the condition that failed'

# ===========================================================================
# Case 5 — SECURITY: cri.toml carries registry auth and this output goes to CI
# logs. No path through the script may print it — including the failure paths,
# which are the tempting place to dump "the config we read".
# ===========================================================================
for node in good empty unset missing; do
  output="$(run_script TALOS_NODES="${node}" 2>&1 || true)"
  refute_text "${output}" "${canary}" "case 5: leaked the registry credential from ${node}'s config into output"
  refute_text "${output}" 'password' "case 5: echoed a config auth key from ${node}"
done

# ===========================================================================
# Case 6 — a mixed fleet fails, and reports EVERY bad node rather than stopping
# at the first. A partial rollout that left one node unverified is precisely the
# case a first-failure exit would hide.
# ===========================================================================
if output="$(run_script TALOS_NODES=good,empty,unset 2>&1)"; then
  fail 'case 6: a fleet containing an unenforcing node MUST fail'
fi
require_text "${output}" 'OK   good' 'case 6: still reports the healthy node'
require_text "${output}" 'FAIL empty' 'case 6: reports the empty-bindir node'
require_text "${output}" 'FAIL unset' 'case 6: reports the unconfigured node'
require_text "${output}" '2 of 3 node(s) cannot enforce' 'case 6: counts every failure, not just the first'

# ===========================================================================
# Case 7 — node discovery. With no --nodes/TALOS_NODES the script reads the
# cluster, so autoscaled nodes are covered without anyone maintaining a list.
# The autoscaler node was central to #2856's evidence.
# ===========================================================================
cat >"${fixtures}/nodes.json" <<'EOF'
{
  "items": [
    {"status": {"addresses": [{"type": "Hostname", "address": "prod-worker-1"}, {"type": "InternalIP", "address": "good"}]}},
    {"status": {"addresses": [{"type": "InternalIP", "address": "empty"}]}}
  ]
}
EOF

if output="$(run_script 2>&1)"; then
  fail 'case 7: discovery must surface the unenforcing discovered node'
fi
require_text "${output}" 'OK   good' 'case 7: discovered the healthy node'
require_text "${output}" 'FAIL empty' 'case 7: discovered the unenforcing node'
refute_text "${output}" 'prod-worker-1' 'case 7: used InternalIP, not Hostname'

# ===========================================================================
# Case 8 — a verifier in ONE containerd must not vouch for the other.
#
# Both config files exist on every prod node (measured), and the two instances
# pull different images: the CRI one pulls workload images, the system one pulls
# Talos' own. So a node wired up on one side and bare on the other is HALF
# unprotected — and an implementation that stops at the first config declaring a
# bin_dir reports it as healthy. Both directions are pinned, because the
# short-circuit only ever hides whichever file is read second.
# ===========================================================================
write_node systembare
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/systembare/files/_etc_cri_conf.d_cri.toml"
no_verifier_config >"${fixtures}/systembare/files/_etc_containerd_config.toml"
write_dir systembare /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

if output="$(run_script TALOS_NODES=systembare 2>&1)"; then
  fail 'case 8: a bare SYSTEM containerd must fail even when the CRI one is wired up'
fi
require_text "${output}" '/etc/containerd/config.toml' 'case 8: names the unprotected containerd'
require_text "${output}" 'OK   systembare [/etc/cri/conf.d/cri.toml]' 'case 8: still reports the wired-up one'

write_node cribare
no_verifier_config >"${fixtures}/cribare/files/_etc_cri_conf.d_cri.toml"
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/cribare/files/_etc_containerd_config.toml"
write_dir cribare /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

if output="$(run_script TALOS_NODES=cribare 2>&1)"; then
  fail 'case 8: a bare CRI containerd must fail even when the system one is wired up'
fi
require_text "${output}" '/etc/cri/conf.d/cri.toml' 'case 8: names the unprotected containerd'

# ===========================================================================
# Case 9 — a row whose LABEL column is empty must still be parsed. Real
# `talosctl ls -l` output on this cluster omits the label for some files, which
# breaks any parser that counts columns from the left past MODE.
# ===========================================================================
write_node nolabel
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/nolabel/files/_etc_cri_conf.d_cri.toml"
write_dir nolabel /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug 11 19:39:51                                              cosign-verifier
EOF

output="$(run_script TALOS_NODES=nolabel 2>&1)" ||
  fail 'case 9: an executable whose ls row has no SELinux label must still be counted'
require_text "${output}" 'holds 1 executable' 'case 9: counted the unlabelled executable'

# ===========================================================================
# Case 10 — a NON-executable file in bin_dir does not satisfy the check.
# containerd executes the binaries there; a stray README enforces nothing.
# ===========================================================================
write_node nonexec
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/nonexec/files/_etc_cri_conf.d_cri.toml"
write_dir nonexec /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   -rw-r--r--   0     0     42        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   README
EOF

if output="$(run_script TALOS_NODES=nonexec 2>&1)"; then
  fail 'case 10: a bin_dir holding only a non-executable file MUST fail'
fi
require_text "${output}" 'holds no executable' 'case 10: names the condition that failed'

# ===========================================================================
# Case 12 — a config file that does NOT exist must be skipped, not fatal.
#
# Every other fixture here has both config files present, which is also true of
# today's nodes — so this path is reachable only on a node image where one is
# absent, and it would then abort the whole run at an assignment under
# `set -e` + `pipefail` rather than reporting anything. A checker that dies
# silently on the drift it exists to watch is worse than no checker.
# ===========================================================================
write_node onlycri
no_verifier_config >"${fixtures}/onlycri/files/_etc_cri_conf.d_cri.toml"
# deliberately NO _etc_containerd_config.toml fixture

if output="$(run_script TALOS_NODES=onlycri 2>&1)"; then
  fail 'case 12: a node with no bin_dir anywhere must FAIL, not pass'
fi
require_text "${output}" 'FAIL onlycri' 'case 12: reported the node instead of aborting on the missing config file'
require_text "${output}" 'no bin_dir in the io.containerd.image-verifier.v1.bindir table' 'case 12: reached the real verdict'

# The mirror image: a missing FIRST file must not stop the second from being
# read, or a verifier declared only in the system config would be invisible.
write_node onlysystem
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/onlysystem/files/_etc_containerd_config.toml"
write_dir onlysystem /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

run_script TALOS_NODES=onlysystem >/dev/null 2>&1 ||
  fail 'case 12: a missing first config file must not stop the second being read'

# ===========================================================================
# Case 11 — KUBECTL_CONTEXT is forwarded to kubectl. The restored kubeconfig's
# current-context is not guaranteed to be prod, which is why the deploy
# composite pins --context; a check that silently read the wrong cluster would
# report a healthy fleet that is not the one being gated.
# ===========================================================================
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/kubectl-args"
cat "${FIXTURES}/nodes.json"
FAKE
chmod +x "${fake_bin}/kubectl"

: >"${fixtures}/kubectl-args"
run_script KUBECTL_CONTEXT=admin@prod >/dev/null 2>&1 || true
grep -Fq -- '--context admin@prod' "${fixtures}/kubectl-args" ||
  fail 'case 11: KUBECTL_CONTEXT was not forwarded to kubectl'

# ...and is omitted entirely when unset, rather than passed as an empty flag,
# which kubectl rejects.
: >"${fixtures}/kubectl-args"
run_script >/dev/null 2>&1 || true
refute_text "$(cat "${fixtures}/kubectl-args")" '--context' 'case 11: passed an empty --context when KUBECTL_CONTEXT was unset'

# ===========================================================================
# Case 16 — a bin_dir holding ONLY a SUBDIRECTORY must FAIL. A directory's mode
# string ('drwxr-xr-x') contains an 'x', so a check that counts any entry with
# an 'x' reports this node as enforcing. containerd executes files in bin_dir
# and does not descend, so such a node permits every pull — a fail-open in the
# dangerous direction, and the exact class #2856 was.
# ===========================================================================
write_node dironly
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/dironly/files/_etc_cri_conf.d_cri.toml"
write_dir dironly /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   drwxr-xr-x   0     0     37        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   .
10.0.1.4   drwxr-xr-x   0     0     37        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   plugins
EOF

if output="$(run_script TALOS_NODES=dironly 2>&1)"; then
  fail 'case 16: a bin_dir holding only a subdirectory MUST fail — directories are not verifier binaries'
fi
require_text "${output}" 'holds no executable' 'case 16: names the condition that failed'

# ...while an executable SYMLINK in bin_dir still counts: a verifier installed
# via a symlink into bin_dir is executed exactly like a regular file.
write_node symlink
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/symlink/files/_etc_cri_conf.d_cri.toml"
write_dir symlink /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   lrwxrwxrwx   0     0     31        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

output="$(run_script TALOS_NODES=symlink 2>&1)" ||
  fail 'case 16: an executable symlink in bin_dir must count as a verifier'
require_text "${output}" 'holds 1 executable' 'case 16: counted the symlinked verifier'

# ===========================================================================
# Case 13 — an empty entry in the node list is a USAGE error (exit 2), not a
# node to skip. Skipping it would check fewer nodes than asked for and still
# exit 0: a clean report on a fleet the check never looked at.
# ===========================================================================
# A trailing comma is the case a post-split scan structurally cannot catch:
# `read -a` discards the trailing empty field, so 'good,' splits to exactly one
# element and looks perfectly well-formed. Leading and doubled commas are here
# for the same reason — the check is on the raw string, so all three share a
# code path and all three are pinned.
for malformed in 'good,,good' 'good,' ',good' 'good,,'; do
  status=0
  output="$(run_script "TALOS_NODES=${malformed}" 2>&1)" || status=$?
  [[ "${status}" -ne 0 ]] ||
    fail "case 13: malformed node list '${malformed}' must not succeed"
  [[ "${status}" -eq 2 ]] ||
    fail "case 13: malformed node list '${malformed}' must exit 2 (usage), got ${status}"
  require_text "${output}" 'empty entry' "case 13: names the malformed list for '${malformed}'"
  refute_text "${output}" 'can enforce image verification.' \
    "case 13: reported a verdict for the malformed list '${malformed}'"
done

# The well-formed list must still work — otherwise the guard above could be
# rejecting everything and every assertion here would still pass.
run_script TALOS_NODES=good,good >/dev/null 2>&1 ||
  fail 'case 13: a well-formed comma-separated list must still be accepted'

# ===========================================================================
# Case 14 — a bin_dir belonging to a DIFFERENT plugin must not count.
#
# `bin_dir` is a generic TOML key. containerd consults only the one under
# `io.containerd.image-verifier.v1.bindir` when verifying images, so a match
# anywhere else reports a node as enforcing on the strength of a setting that
# has nothing to do with verification — a fail-open produced by the parser
# rather than by the cluster.
# ===========================================================================
write_node otherplugin
cat >"${fixtures}/otherplugin/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.registry.configs.'ghcr.io'.auth]
      password = '${canary}'
      username = 'devantler'

  [plugins.'io.containerd.nri.v1.nri']
    bin_dir = '/opt/nri/bin'
EOF
# The unrelated plugin's directory is fully populated: if the check were to read
# it, it would report a healthy node. Only the SCOPE keeps this failing.
write_dir otherplugin /opt/nri/bin <<EOF
${listing_header}
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   nri-plugin
EOF

if output="$(run_script TALOS_NODES=otherplugin 2>&1)"; then
  fail 'case 14: a bin_dir from an unrelated plugin table MUST NOT satisfy the check'
fi
require_text "${output}" 'no bin_dir in the io.containerd.image-verifier.v1.bindir table' 'case 14: reached the real verdict'
refute_text "${output}" '/opt/nri/bin' 'case 14: read a bin_dir from the wrong table'

# ===========================================================================
# Case 15 — an UNREACHABLE node aborts; it is never reported as checked.
#
# The failure this guards is quiet: if a config file's absence is inferred from
# a failed read, a node the runner cannot talk to looks identical to one that
# simply does not ship that file. Every config would be "absent", the node would
# be skipped, and the fleet summary would report a clean result for a node
# nothing ever looked at.
# ===========================================================================
status=0
output="$(run_script TALOS_NODES=unreachable 2>&1)" || status=$?
if [[ "${status}" -ne 2 ]]; then
  fail "case 15: an unreachable node must exit 2 (infrastructure), got ${status}"
fi
require_text "${output}" 'cannot reach node unreachable' 'case 15: names the unreachable node'
refute_text "${output}" 'can enforce image verification.' 'case 15: reported a verdict for a node it never checked'

# ===========================================================================
# Case 17 — a node that stops answering AFTER its config was read is an
# infrastructure error, not a verdict.
#
# `talosctl ls -l` on a bin_dir already proven to exist can still fail. awk over
# empty input prints 0 quite happily, so discarding that status turns an
# unreachable node into a confident "holds no executable" FAIL — the checker
# blaming the cluster for a runner-side fault, at exit code 1 where a human
# reads "the fleet is unprotected" rather than "the check did not run".
# ===========================================================================
write_node vanishing
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/vanishing/files/_etc_cri_conf.d_cri.toml"
# `ls <dir>` succeeds (the existence probe) because a listing fixture exists,
# but it is EMPTY, so `ls -l` on it produces no rows...
write_dir vanishing /opt/containerd/image-verifier/bin </dev/null
# ...and this makes the long-form listing fail outright, which is the real
# shape: the directory was there a moment ago and the node is now unresponsive.
printf 'FAIL_LS_LONG\n' >"${fixtures}/vanishing/lsfail"

status=0
output="$(run_script TALOS_NODES=vanishing 2>&1)" || status=$?
if [[ "${status}" -eq 1 ]]; then
  fail 'case 17: a failed listing on an existing bin_dir must not be reported as "holds no executable"'
fi
[[ "${status}" -eq 2 ]] ||
  fail "case 17: a failed listing must exit 2 (infrastructure), got ${status}"
require_text "${output}" 'could not list' 'case 17: names the listing failure'
refute_text "${output}" 'can enforce image verification.' 'case 17: reported a verdict it could not reach'

# ===========================================================================
# Case 19 — a node that drops out between the config read and the bin_dir probe
# is an infrastructure error, not "the directory does not exist".
#
# `directory_exists` sees the same failed `ls` either way. Reporting the
# unreachable case as a verdict blames the cluster for a runner-side fault, and
# a start-of-run reachability probe cannot cover it — the node was answering
# when the run began.
# ===========================================================================
write_node dropout
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/dropout/files/_etc_cri_conf.d_cri.toml"
printf 'DROPPED\n' >"${fixtures}/dropout/lsfail_all"

status=0
output="$(run_script TALOS_NODES=dropout 2>&1)" || status=$?
if [[ "${status}" -eq 1 ]]; then
  fail 'case 19: an unreachable node must not be reported as "bin_dir does not exist"'
fi
[[ "${status}" -eq 2 ]] ||
  fail "case 19: a mid-check dropout must exit 2 (infrastructure), got ${status}"
require_text "${output}" 'cannot reach node dropout' 'case 19: names the unreachable node'
refute_text "${output}" 'does not exist' 'case 19: blamed the cluster for a runner-side fault'

# ===========================================================================
# Case 18 — PARTIAL node discovery must abort, never pass on the prefix.
#
# kubectl can return a valid node followed by one whose addresses are missing,
# so jq emits an address and THEN fails. Run through a process substitution,
# `fail_infra` exits only that subshell: the loop keeps the addresses already
# emitted, and if those happen to be healthy the script exits 0 having silently
# dropped the rest of the fleet. That is the fail-open this whole script exists
# to detect, arriving through the enumeration rather than the verdict.
# ===========================================================================
# kubectl SUCCEEDS here — that is the whole point. The failure has to come from
# jq, midway through valid output: the first node yields an address, the second
# has no `status.addresses` at all, so jq errors on it AFTER writing "good" to
# stdout. A fake that simply exits non-zero would abort discovery at the kubectl
# step and never exercise the partial-output path (it did, and the ablation
# caught it).
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' '{"items":[{"status":{"addresses":[{"type":"InternalIP","address":"good"}]}},{"status":{}}]}'
exit 0
FAKE
chmod +x "${fake_bin}/kubectl"

status=0
output="$(run_script 2>&1)" || status=$?
if [[ "${status}" -eq 0 ]]; then
  fail 'case 18: partial node discovery MUST NOT exit 0 — the rest of the fleet was never enumerated'
fi
[[ "${status}" -eq 2 ]] ||
  fail "case 18: partial discovery must exit 2 (infrastructure), got ${status}"
refute_text "${output}" 'can enforce image verification.' 'case 18: reported a fleet it only partly enumerated'

# ===========================================================================
# Case 22 — a discovered node with NO InternalIP must not be silently dropped.
#
# Distinct from case 18, and the distinction is the whole point: there the node
# has no `status.addresses` at all, so jq ERRORS and discovery already fails
# closed. Here the array is present and perfectly valid — it just holds only a
# Hostname and an ExternalIP. jq's `select(.type == "InternalIP")` matches
# nothing, emits nothing, and SUCCEEDS. The node vanishes from the fleet while
# every other node reports OK, so the script prints a confident green verdict
# for a fleet it never fully enumerated: the same fail-open shape the script
# exists to detect, arriving through enumeration rather than the verdict.
# ===========================================================================
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' '{"items":[
  {"metadata":{"name":"prod-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"good"}]}},
  {"metadata":{"name":"prod-worker-2"},"status":{"addresses":[{"type":"Hostname","address":"prod-worker-2"},{"type":"ExternalIP","address":"203.0.113.7"}]}}
]}'
exit 0
FAKE
chmod +x "${fake_bin}/kubectl"

status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -ne 0 ]] ||
  fail 'case 22: a node with no InternalIP MUST NOT yield exit 0 — it was never checked'
[[ "${status}" -eq 2 ]] ||
  fail "case 22: a node with no InternalIP must exit 2 (infrastructure), got ${status}"
require_text "${output}" 'prod-worker-2' 'case 22: names the node that has no InternalIP'
refute_text "${output}" 'can enforce image verification.' \
  'case 22: reported a verdict for a fleet that was only partly enumerated'

# The healthy-fleet control: without it, a discover_nodes that rejected EVERY
# node would pass every assertion above.
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' '{"items":[
  {"metadata":{"name":"prod-worker-1"},"status":{"addresses":[{"type":"Hostname","address":"prod-worker-1"},{"type":"InternalIP","address":"good"}]}}
]}'
exit 0
FAKE
chmod +x "${fake_bin}/kubectl"
run_script >/dev/null 2>&1 ||
  fail 'case 22: a fleet whose nodes all have an InternalIP must still be accepted'

# ===========================================================================
# Case 20 — an explicitly EMPTY node list is a usage error, not a fallback.
#
# `--nodes "$TALOS_NODES"` with the variable unset expands to an empty string.
# Treating that as "no list was given" silently switches to cluster discovery,
# so the check reports a green verdict for a DIFFERENT fleet than the caller
# asked for — and the explicit addresses are usually explicit precisely because
# the current kube context is not the fleet in question.
#
# Discovery is wired to a healthy node here on purpose: if the fallback fires,
# the script exits 0 and the assertion below is what catches it.
# ===========================================================================
run_script_with_args() {
  env PATH="${fake_bin}:${PATH}" FIXTURES="${fixtures}" \
    TALOSCTL="${fake_bin}/talosctl" KUBECTL="${fake_bin}/kubectl" \
    bash "${script}" "$@"
}

status=0
output="$(run_script_with_args --nodes '' 2>&1)" || status=$?
[[ "${status}" -ne 0 ]] ||
  fail 'case 20: --nodes "" must not silently fall back to cluster discovery'
[[ "${status}" -eq 2 ]] ||
  fail "case 20: --nodes '' must exit 2 (usage), got ${status}"
refute_text "${output}" 'can enforce image verification.' \
  'case 20: reported a verdict for a fleet the caller never asked for'

# Same for the environment form, which is how CI passes the list.
status=0
output="$(run_script TALOS_NODES= 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] ||
  fail "case 20: TALOS_NODES set-but-empty must exit 2 (usage), got ${status}"

# Controls: an explicit non-empty list still works, and a genuinely UNSET
# TALOS_NODES must still reach discovery — otherwise the guard above would be
# rejecting the discovery path outright and case 7 would be the only thing
# keeping it honest.
run_script_with_args --nodes good >/dev/null 2>&1 ||
  fail 'case 20: an explicit non-empty --nodes must still be accepted'
run_script >/dev/null 2>&1 ||
  fail 'case 20: an unset TALOS_NODES must still fall through to discovery'

# ===========================================================================
# Case 21 — a config probe that fails for a reason OTHER than "absent" is an
# infrastructure error, never a config to skip.
#
# `talosctl ls <config>` failing is ambiguous: the file may not be there, or the
# node may have hiccuped. Only the first is a verdict. Treating both as "this
# node does not ship that config" means a transient fault silently removes one
# containerd from the check — and if the OTHER containerd is wired up,
# `configs_present` stays nonzero and the node passes while one of its two image
# paths was never inspected.
#
# The system containerd here is deliberately HEALTHY: that is what makes the
# skip invisible without this case.
# ===========================================================================
write_node probeerror
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/probeerror/files/_etc_containerd_config.toml"
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/probeerror/files/_etc_cri_conf.d_cri.toml"
write_dir probeerror /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.9   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF
# The CRI config probe fails with an RPC error rather than an lstat one. The
# node itself still answers `ls /`, so a reachability probe cannot catch this.
printf 'PROBE_ERROR\n' >"${fixtures}/probeerror/lserror_etc_cri_conf.d_cri.toml"

status=0
output="$(run_script TALOS_NODES=probeerror 2>&1)" || status=$?
[[ "${status}" -ne 0 ]] ||
  fail 'case 21: a config probe error MUST NOT be skipped into a green verdict'
[[ "${status}" -eq 2 ]] ||
  fail "case 21: a config probe error must exit 2 (infrastructure), got ${status}"
require_text "${output}" '/etc/cri/conf.d/cri.toml' 'case 21: names the config it could not probe'
refute_text "${output}" 'can enforce image verification.' \
  'case 21: reported a verdict for a node whose containerd was never inspected'
refute_text "${output}" "${canary}" 'case 21: probe error must not carry config contents'

# Control: with the probe error removed the same node passes, so the assertions
# above are pinning the probe failure and not some unrelated defect in the
# fixture.
rm -f "${fixtures}/probeerror/lserror_etc_cri_conf.d_cri.toml"
run_script TALOS_NODES=probeerror >/dev/null 2>&1 ||
  fail 'case 21: the same node must pass once the probe error is gone'

printf 'PASS: all cases\n'
