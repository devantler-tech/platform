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
      printf '%s %s\n' "${node}" "${path}" >>"${FIXTURES}/read-log"
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
      # The node stops answering the REACHABILITY probe specifically, while its file
      # probes still resolve normally. That ordering is what reaches the departure
      # check on the path_probe arms: those arms need a probe that returned a verdict
      # (absent, or a non-lstat error) followed by a node that is no longer there --
      # which `lsfail_all` cannot produce, because it fails the file probe too.
      if [[ "${path}" == '/' && -f "${FIXTURES}/${node}/lsfail_root" ]]; then
        printf 'error connecting to %s\n' "${node}" >&2
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

# Serves a SEQUENCE when one is staged: the Nth `kubectl get nodes` of a run is
# answered from nodes.<N>.json when that file exists, and from nodes.json
# otherwise. The script reads the inventory once before the checks and again
# after them, so a per-call answer is what lets a test move the fleet in the
# window between those two reads -- the autoscaler race the convergence loop
# exists to close. Cases that stage no sequence are unaffected.
# Defined once and reused: later cases replace the fake with static variants, so
# any case needing the sequenced behaviour back calls this rather than pasting a
# second copy that could drift from this one.
install_sequenced_kubectl() {
  cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
call_count_file="${FIXTURES}/kubectl-call-count"
call_number=$(( $(cat "${call_count_file}" 2>/dev/null || printf '0') + 1 ))
printf '%s' "${call_number}" >"${call_count_file}"
if [[ -f "${FIXTURES}/nodes.${call_number}.json" ]]; then
  cat "${FIXTURES}/nodes.${call_number}.json"
else
  cat "${FIXTURES}/nodes.json"
fi
FAKE
  chmod +x "${fake_bin}/kubectl"
}

install_sequenced_kubectl

# Clears both the counter and any staged sequence, so one case's fleet cannot
# leak into the next.
reset_node_sequence() {
  rm -f "${fixtures}/kubectl-call-count" "${fixtures}"/nodes.[0-9].json
}

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

# A literal backslash, BUILT rather than typed, so no layer between this file and
# the fixture can decode a TOML escape sequence on the way through.
BS="$(awk 'BEGIN{printf "%c", 92}')"
readonly BS

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
    {"metadata": {"name": "prod-worker-1", "uid": "uid-good"}, "status": {"addresses": [{"type": "Hostname", "address": "prod-worker-1"}, {"type": "InternalIP", "address": "good"}]}},
    {"metadata": {"name": "prod-worker-2", "uid": "uid-empty"}, "status": {"addresses": [{"type": "InternalIP", "address": "empty"}]}}
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
printf '%s\n' '{"items":[{"metadata":{"name":"prod-worker-1","uid":"uid-good"},"status":{"addresses":[{"type":"InternalIP","address":"good"}]}},{"metadata":{"name":"prod-worker-2","uid":"uid-2"},"status":{}}]}'
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
  {"metadata":{"name":"prod-worker-1","uid":"uid-1"},"status":{"addresses":[{"type":"InternalIP","address":"good"}]}},
  {"metadata":{"name":"prod-worker-2","uid":"uid-2"},"status":{"addresses":[{"type":"Hostname","address":"prod-worker-2"},{"type":"ExternalIP","address":"203.0.113.7"}]}}
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
  {"metadata":{"name":"prod-worker-1","uid":"uid-1"},"status":{"addresses":[{"type":"Hostname","address":"prod-worker-1"},{"type":"InternalIP","address":"good"}]}}
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

# ===========================================================================
# Case 23 — two nodes publishing the SAME InternalIP must not pass.
#
# The flattening emits that address once per node, so both passes inspect the
# SAME machine while the other node is never looked at — and since the machine
# they both reach is healthy, the run reports a green fleet with a node it never
# touched. Distinct from case 22: there the node contributes NO address, here it
# contributes a duplicate one, so a "did every node yield an address?" check
# passes and only a uniqueness check catches it.
#
# `scripts/refresh-flux-ghcr-auth.sh`'s `validate_talos_node_inventory` refuses
# the same shape before it mutates anything; this mirrors that rule.
# ===========================================================================
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' '{"items":[
  {"metadata":{"name":"prod-worker-1","uid":"uid-1"},"status":{"addresses":[{"type":"InternalIP","address":"good"}]}},
  {"metadata":{"name":"prod-worker-stale","uid":"uid-stale"},"status":{"addresses":[{"type":"InternalIP","address":"good"}]}}
]}'
exit 0
FAKE
chmod +x "${fake_bin}/kubectl"

status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -ne 0 ]] ||
  fail 'case 23: two nodes sharing one InternalIP MUST NOT yield exit 0 — one was never inspected'
[[ "${status}" -eq 2 ]] ||
  fail "case 23: a duplicate InternalIP must exit 2 (infrastructure), got ${status}"
require_text "${output}" 'prod-worker-stale' 'case 23: names the nodes sharing an address'
refute_text "${output}" 'can enforce image verification.' \
  'case 23: reported a verdict for a fleet where one node was never inspected'

# A node carrying TWO InternalIPs is the same ambiguity from the other side:
# which one talosctl would reach is unspecified, so it is refused rather than
# guessed.
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' '{"items":[
  {"metadata":{"name":"prod-worker-dual","uid":"uid-dual"},"status":{"addresses":[{"type":"InternalIP","address":"good"},{"type":"InternalIP","address":"empty"}]}}
]}'
exit 0
FAKE
chmod +x "${fake_bin}/kubectl"
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] ||
  fail "case 23: a node with two InternalIPs must exit 2 (infrastructure), got ${status}"
require_text "${output}" 'prod-worker-dual' 'case 23: names the node carrying two addresses'

# Control: distinct addresses across distinct nodes still pass, so the
# uniqueness rule is not simply rejecting every multi-node fleet.
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' '{"items":[
  {"metadata":{"name":"prod-worker-1","uid":"uid-1"},"status":{"addresses":[{"type":"InternalIP","address":"good"}]}},
  {"metadata":{"name":"prod-worker-2","uid":"uid-2"},"status":{"addresses":[{"type":"InternalIP","address":"onlysystem"}]}}
]}'
exit 0
FAKE
chmod +x "${fake_bin}/kubectl"
# Assert a verdict line for EACH address, not just exit 0: a regression that
# silently drops one discovered node from the fleet still exits 0, so an
# exit-status-only control would keep passing while enumeration shrank.
output="$(run_script 2>&1)" ||
  fail 'case 23: a fleet of distinct nodes with distinct InternalIPs must still be accepted'
require_text "${output}" 'good' 'case 23: control reports the first discovered node'
require_text "${output}" 'onlysystem' 'case 23: control reports the second discovered node'

# ===========================================================================
# Case 24 — the plugin is CONFIGURED and DISABLED. A bin_dir that is set,
# present and full of executables proves nothing if containerd never loads the
# plugin that runs them, so this node must FAIL exactly like one with no
# verifier at all. Everything else about it is deliberately healthy, so a pass
# here could only come from not looking at disabled_plugins.
# ===========================================================================
write_node disabled
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.registry.configs.'ghcr.io'.auth]
      password = '${canary}'
      username = 'devantler'

  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
write_dir disabled /opt/containerd/image-verifier/bin <<EOF
${listing_header}
disabled    -rwxr-xr-x   0     0     1024      2026-08-01T00:00:00Z                                                 .
disabled    -rwxr-xr-x   0     0     1024      2026-08-01T00:00:00Z                                                 verifier
EOF

if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a disabled image-verifier plugin must fail the node'
fi
require_text "${output}" 'FAIL disabled' 'case 24: names the node that cannot enforce'
require_text "${output}" 'disabled_plugins' 'case 24: says WHY, so the operator edits the right setting'
refute_text "${output}" "${canary}" 'case 24: never prints registry credentials'

# Control: the SAME node with the plugin left enabled passes. Without this, the
# case above would also pass if the script had simply started failing this
# fixture for some unrelated reason.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['io.containerd.snapshotter.v1.blockfile']

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.registry.configs.'ghcr.io'.auth]
      password = '${canary}'
      username = 'devantler'

  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24 control: an UNRELATED disabled plugin must not fail the node'
require_text "${output}" 'OK   disabled' 'case 24 control: the node enforces'

# A multi-line array is the shape containerd config actually uses once more than
# one plugin is off; a single-line matcher would miss it and report the node OK.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = [
  'io.containerd.snapshotter.v1.blockfile',
  'io.containerd.image-verifier.v1.bindir',
]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a disabled plugin listed in a MULTI-LINE array must still fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 24: multi-line array is read'

# A COMMENT LINE inside the multi-line array. The lines are concatenated into one
# buffer, so a comment that is not stripped swallows the entry on the next line:
# that entry lands inside the comment text, fails the quote test, and is skipped.
# The node then reads as ENABLED while containerd has the verifier DISABLED --
# a false OK on the one check whose whole job is to catch exactly that.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = [
  'io.containerd.snapshotter.v1.blockfile',
  # image verification is off while we debug the registry
  'io.containerd.image-verifier.v1.bindir',
]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a comment line before the entry must not hide a disabled plugin'
fi
require_text "${output}" 'disabled_plugins' 'case 24: commented multi-line array is read'

# The mirror image: a # INSIDE a quoted entry is content, not a comment, so
# stripping must not truncate the entry and lose the plugin that follows it.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = [
  'io.containerd.snapshotter.v1.blockfile#notacomment', 'io.containerd.image-verifier.v1.bindir',
]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a # inside a quoted entry must not truncate the array'
fi
require_text "${output}" 'disabled_plugins' 'case 24: quoted # is content, not a comment'

# Same class one character along: a ] INSIDE a quoted entry is content, not the
# array terminator. Ending the array there drops every entry that follows --
# including ours -- so the script reported disabled=0 and OK while containerd
# had the verifier switched off. That is a FAIL-OPEN on the one signal meant to
# mean enforcement is on, which is the failure mode this script exists to catch.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = [
  'io.containerd.snapshotter.v1.blockfile]notaterminator', 'io.containerd.image-verifier.v1.bindir',
]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a ] inside a quoted entry must not terminate the array'
fi
require_text "${output}" 'disabled_plugins' 'case 24: quoted ] is content, not the array terminator'

# A `disabled_plugins` key that belongs to some OTHER table is not the root key
# and must not disable anything -- otherwise an unrelated setting could switch
# this whole check off.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3

[plugins]
  [plugins.'io.containerd.some.other.plugin']
    disabled_plugins = ['io.containerd.image-verifier.v1.bindir']

  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: a non-root disabled_plugins key must not be treated as the root one'
require_text "${output}" 'OK   disabled' 'case 24: scoped to the root key'

# A DIFFERENT plugin whose name merely CONTAINS ours must not disable us. This
# is a false-FAIL guard: a substring test over the array text fails a node whose
# verifier is perfectly enabled, which is the worst direction for a check whose
# whole output is "enforcement is off".
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['example.io.containerd.image-verifier.v1.bindir-extra']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: a different plugin whose name contains ours must not read as disabled'
require_text "${output}" 'OK   disabled' 'case 24: entries are matched exactly, not by substring'

# A quoted key that merely NORMALISES to ours is a DIFFERENT key. TOML treats a
# bare and a quoted key as the same only when they spell the same characters, so
# 'disabled_ plugins' -- with an interior space -- is a distinct setting that
# containerd never reads. Deleting whitespace and quotes before comparing aliased
# it onto ours and failed a node whose verifier is enabled: a false FAIL, the same
# direction as the substring guard above and the worst one for a check whose only
# output is "enforcement is off".
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
'disabled_ plugins' = ['io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: a quoted lookalike key must not be aliased onto the root key'
require_text "${output}" 'OK   disabled' 'case 24: interior characters inside a quoted key are preserved'

# A BASIC (double-quoted) TOML key interprets escapes, so this names our setting and
# containerd switches the verifier OFF. Comparing the raw text missed it and the node
# reported OK with no enforcement -- a FAIL-OPEN, the direction that matters here.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
"disabled\u005fplugins" = ['io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: an escaped basic-quoted key must be decoded before comparison'
fi
require_text "${output}" 'disabled_plugins' 'case 24: TOML basic-string escapes are decoded in a quoted key'

# The LITERAL (single-quoted) spelling of the same text is a DIFFERENT key: literal
# strings do not interpret escapes, so containerd never reads it. Decoding it too would
# re-introduce the aliasing this parser exists to avoid. Measured with go-toml.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
'disabled\u005fplugins' = ['io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: a literal-quoted key with escape text must not be decoded onto the root key'
require_text "${output}" 'OK   disabled' 'case 24: literal strings keep escape text verbatim'

# The SAME escape hole one level down: the ENTRY VALUES in the array. A basic-quoted
# entry interprets escapes, so this names our plugin and containerd switches the verifier
# off, while an un-decoded comparison misses it and the node reports OK -- a FAIL-OPEN.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["io.containerd.image-verifier.v1\u002ebindir"]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: an escaped basic-quoted ENTRY must be decoded before comparison'
fi
require_text "${output}" 'disabled_plugins' 'case 24: TOML basic-string escapes are decoded in an array entry'

# And the LITERAL spelling of the same entry is a DIFFERENT plugin id that containerd
# never matches, so it must NOT disable us.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['io.containerd.image-verifier.v1\u002ebindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: a literal-quoted entry with escape text must not be decoded onto our plugin id'
require_text "${output}" 'OK   disabled' 'case 24: literal entries keep escape text verbatim'

# A MULTILINE BASIC entry ("""...""") is the same plugin id to containerd: go-toml
# resolves the triple-quoted form to exactly the characters of our identifier, so
# the verifier is OFF. Treating each quote as a single-char delimiter closed the
# string on the SECOND quote of the opener and compared an EMPTY entry, so the node
# reported OK with no enforcement -- the fail-open this script exists to prevent.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["""io.containerd.image-verifier.v1.bindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a MULTILINE BASIC entry must be read as our plugin id'
fi
require_text "${output}" 'disabled_plugins' 'case 24: triple-quoted basic entries are parsed'

# The MULTILINE LITERAL spelling ('''...''') resolves to the SAME characters --
# measured with go-toml, which returns our identifier for the basic, the literal
# and the plain forms alike. The reviewer named only the basic form; this half of
# the hole is identical and fails open in exactly the same direction.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['''io.containerd.image-verifier.v1.bindir''']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a MULTILINE LITERAL entry must be read as our plugin id'
fi
require_text "${output}" 'disabled_plugins' 'case 24: triple-quoted literal entries are parsed'

# A multiline BASIC entry interprets escapes exactly as the single-line form does,
# so the escaped spelling is still our plugin id and must disable the node.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["""io.containerd.image-verifier.v1${BS}u002ebindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: escapes inside a MULTILINE BASIC entry must be decoded'
fi
require_text "${output}" 'disabled_plugins' 'case 24: multiline basic escapes are decoded'

# ...and the multiline LITERAL form interprets nothing, so the same text is a
# DIFFERENT plugin id containerd never matches. Decoding it would alias a healthy
# node onto ours and report a false FAIL.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['''io.containerd.image-verifier.v1${BS}u002ebindir''']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: a multiline LITERAL entry with escape text must not be decoded onto our plugin id'
require_text "${output}" 'OK   disabled' 'case 24: multiline literal entries keep escape text verbatim'

# A MULTILINE string that genuinely SPANS PHYSICAL LINES. TOML trims a newline that
# comes straight after the opening delimiter, so this names our plugin EXACTLY and
# containerd has the verifier off -- measured with go-toml, which returns the bare id for
# both the basic and the literal form.
#
# The walk restarted its quote state on every line, so the closing delimiter on the second
# line read as a fresh OPENER; and the lines were joined with a SPACE, which turned the
# newline TOML trims into a leading space. The entry compared as something that is not our
# id and the node reported OK with no enforcement.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["""
io.containerd.image-verifier.v1.bindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a multiline BASIC entry spanning physical lines must be read as our plugin id'
fi
require_text "${output}" 'disabled_plugins' 'case 24: quote state is carried across array lines'

# The LITERAL triple spans lines under the same rule.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['''
io.containerd.image-verifier.v1.bindir''']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a multiline LITERAL entry spanning physical lines must be read as our plugin id'
fi
require_text "${output}" 'disabled_plugins' 'case 24: literal triples carry across lines too'

# CONTROL: only the FIRST newline after the opener is trimmed. A second one is CONTENT, so
# this entry is "\nio.containerd..." -- a DIFFERENT string that containerd never matches,
# and the node must stay OK. Without this the fix could pass by stripping every leading
# newline, which would alias a different plugin id onto ours.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["""

io.containerd.image-verifier.v1.bindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: only the first newline after a multiline opener is trimmed; a second is content'
require_text "${output}" 'OK   disabled' 'case 24: a retained newline makes it a different plugin id'

# A # inside a multiline string that SPANS lines. The comment stripper restarted its
# quote state on every line, so on the second line the # read as a comment opener and
# truncated the array there -- dropping our entry and reporting OK with the verifier off.
# Carrying the string state across lines is what keeps the # as content. Measured with
# go-toml: this list holds "a#b" AND our plugin id.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["""
a#b""", 'io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a # inside a SPANNING multiline string must not truncate the array'
fi
require_text "${output}" 'disabled_plugins' 'case 24: comment state is carried across array lines'

# A ] inside a multiline string that SPANS lines, in a MULTI-LINE array. The array-
# terminator scan also restarted per line, so the ] on the second line ended the array
# there -- and the entry on the THIRD line was never buffered at all. Our plugin is on
# that third line, so the node reported OK with the verifier off. Measured with go-toml:
# this list holds "a]b" AND our plugin id.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["""
a]b""",
  'io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a ] inside a SPANNING multiline string must not terminate the array'
fi
require_text "${output}" 'disabled_plugins' 'case 24: terminator state is carried across array lines'

# A # inside a MULTILINE string is content, not a comment opener. ONE interior quote
# is legal inside a multiline basic string and makes the quote count ODD, so a walk that
# reads each quote singly ends up OUTSIDE the string at the #, truncates the array there,
# and drops our entry -- reporting OK while containerd has the verifier off.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["""a"b#c""", 'io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a # inside a MULTILINE string must not truncate the array'
fi
require_text "${output}" 'disabled_plugins' 'case 24: # inside a triple-quoted string is content'

# ...and a ] inside a MULTILINE string is not the array terminator. In a MULTI-LINE
# array that early termination stops the buffer before the next line, so our entry on
# it is never examined at all -- the same fail-open, reached through enumeration.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = [
  """a"b]c""",
  'io.containerd.image-verifier.v1.bindir',
]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a ] inside a MULTILINE string must not terminate the array'
fi
require_text "${output}" 'disabled_plugins' 'case 24: ] inside a triple-quoted string is content'

# Our identifier appearing only in a trailing TOML COMMENT is not an entry.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ['io.containerd.snapshotter.v1.blockfile'] # io.containerd.image-verifier.v1.bindir

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 24: our identifier in a comment must not read as disabled'
require_text "${output}" 'OK   disabled' 'case 24: a trailing comment is not an array entry'

# Double-quoted entries are TOML too, and must still be caught.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
disabled_plugins = ["io.containerd.image-verifier.v1.bindir"]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a double-quoted disabled entry must still fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 24: double-quoted entries are read'

# The root KEY may be quoted too. TOML treats a bare key and a quoted key as the
# SAME key -- measured against pelletier/go-toml/v2, containerd's own parser
# family: `disabled_plugins`, `"disabled_plugins"` and `'disabled_plugins'` all
# unmarshal to one key. A bare-key-only match therefore never calls verdict() on
# the quoted spellings, leaving disabled = 0 while containerd has the verifier
# switched OFF -- a node reporting OK with no enforcement, the same fail-open
# class as the quoted `]` and the substring match above.
#
# The table-header rule already strips quotes before comparing; the root key did
# not, and that inconsistency is what this closes.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
"disabled_plugins" = ["io.containerd.image-verifier.v1.bindir"]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a DOUBLE-QUOTED root disabled_plugins key must still fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 24: a double-quoted root key is read'

cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<EOF
version = 3
'disabled_plugins' = ["io.containerd.image-verifier.v1.bindir"]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a SINGLE-QUOTED root disabled_plugins key must still fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 24: a single-quoted root key is read'

# A LARGE config with the disabled key near the top. This is the SIGPIPE case:
# an awk that printed its verdict and exited would close the pipe while
# `talosctl read` was still writing, and under `set -o pipefail` that SIGPIPE is
# indistinguishable from a genuine read failure -- so the node would be reported
# as an INFRASTRUCTURE error (exit 2) rather than the FAIL it is. The verdict has
# to survive reading the whole file.
{
  printf 'version = 3\n'
  printf "disabled_plugins = ['io.containerd.image-verifier.v1.bindir']\n"
  printf '\n[plugins]\n'
  printf "  [plugins.'io.containerd.image-verifier.v1.bindir']\n"
  printf "    bin_dir = '/opt/containerd/image-verifier/bin'\n"
  # Filler well past a pipe buffer, so the producer is still writing when a
  # short-circuiting reader would have quit.
  awk 'BEGIN { for (i = 0; i < 20000; i++) print "# padding line to outrun the pipe buffer" }'
} >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml"

if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 24: a disabled plugin in a LARGE config must still fail the node'
else
  status=$?
fi
require_text "${output}" 'disabled_plugins' 'case 24: large config still reports the disabled plugin'
if [ "${status}" -ne 1 ]; then
  printf 'FAIL: case 24: a disabled plugin in a large config must exit 1 (verdict), got %s — a SIGPIPE from an early awk exit is being reported as an infrastructure error\n%s\n' \
    "${status}" "${output}" >&2
  exit 1
fi
refute_text "${output}" 'talosctl read failed' 'case 24: not misreported as a read failure'

# ===========================================================================
# Case 25 — the node set changes WHILE the fleet is being checked. The
# inventory is read before a serial pass, so an autoscaler that adds or
# replaces a worker mid-run would otherwise leave that machine uninspected
# while every node in the stale snapshot reported healthy.
# ===========================================================================
install_sequenced_kubectl

node_json() { printf '{"metadata":{"name":"%s","uid":"%s"},"status":{"addresses":[{"type":"InternalIP","address":"%s"}]}}' "$1" "$2" "$3"; }

# A worker joins between the pass and the re-read, then the fleet holds still:
# the check re-runs over the new set and reports on what actually exists.
reset_node_sequence
printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.1.json"
for call in 2 3; do
  printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-2 uid-2 onlysystem)" \
    >"${fixtures}/nodes.${call}.json"
done
output="$(run_script 2>&1)" ||
  fail 'case 25: a settled inventory must be reported, not treated as a fault'
require_text "${output}" 'The node set changed while it was being checked' 'case 25: says it re-ran'
require_text "${output}" 'onlysystem' 'case 25: the node that joined mid-run IS inspected'
require_text "${output}" 'All 2 node(s)' 'case 25: the verdict counts the fleet that actually exists'

# The same race with the joiner NOT healthy: re-running has to reach a verdict
# about it, not just re-count the fleet.
reset_node_sequence
printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.1.json"
for call in 2 3; do
  printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-2 uid-2 empty)" \
    >"${fixtures}/nodes.${call}.json"
done
if output="$(run_script 2>&1)"; then
  fail 'case 25: a node that joined mid-run and cannot enforce must fail the run'
fi
require_text "${output}" 'FAIL empty' 'case 25: the mid-run joiner is judged, not just counted'

# An inventory that never settles is bounded rather than looping forever, and
# fails closed instead of reporting on a fleet that never held still.
reset_node_sequence
printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.1.json"
printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-2 uid-2 onlysystem)" >"${fixtures}/nodes.2.json"
printf '{"items":[%s]}' "$(node_json prod-worker-2 uid-2 onlysystem)" >"${fixtures}/nodes.3.json"
if output="$(run_script IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS=2 2>&1)"; then
  fail 'case 25: an inventory that never settles must not report a verdict'
fi
require_text "${output}" 'never held still' 'case 25: says the fleet would not settle'

# The SAME fleet returned in a DIFFERENT order across the two reads must settle,
# not re-run. `kubectl get nodes` guarantees no ordering, so comparing the
# discovery stream as a string would call a re-ordered but identical fleet
# "changed" and could burn the attempt bound into an exit 2 for a cluster that
# never moved. Attempts are pinned at 1 so any re-run at all is a failure.
install_sequenced_kubectl
reset_node_sequence
printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-2 uid-2 onlysystem)" >"${fixtures}/nodes.1.json"
printf '{"items":[%s,%s]}' "$(node_json prod-worker-2 uid-2 onlysystem)" "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.2.json"
output="$(run_script IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS=1 2>&1)" ||
  fail 'case 25: a re-ordered but identical fleet must settle, not be treated as a change'
require_text "${output}" 'All 2 node(s)' 'case 25: the re-ordered fleet is reported normally'
refute_text "${output}" 'The node set changed' 'case 25: re-ordering is not a change'

# ===========================================================================
# Case 26 — an autoscaler replacement that REUSES the retired node's address.
# Address alone cannot tell the two apart, so a machine that was never
# inspected would be reported as one that passed. The UID is the discriminator.
# ===========================================================================
install_sequenced_kubectl
reset_node_sequence
printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-original good)" >"${fixtures}/nodes.1.json"
printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-replacement good)" >"${fixtures}/nodes.2.json"
if output="$(run_script IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS=1 2>&1)"; then
  fail 'case 26: a replacement reusing the address must not pass as the original'
fi
require_text "${output}" 'never held still' 'case 26: the replacement is detected as a change'

# Control: the SAME node, same UID and address, across both reads settles at
# once. Without it, case 26 would also pass if the script had simply started
# rejecting every discovery run.
reset_node_sequence
printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-original good)" >"${fixtures}/nodes.1.json"
printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-original good)" >"${fixtures}/nodes.2.json"
output="$(run_script IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS=1 2>&1)" ||
  fail 'case 26 control: an unchanged fleet must settle on the first attempt'
require_text "${output}" 'All 1 node(s)' 'case 26 control: reports the unchanged fleet'

# A node with no UID at all is refused rather than compared on address alone.
reset_node_sequence
printf '%s\n' '{"items":[{"metadata":{"name":"prod-worker-1"},"status":{"addresses":[{"type":"InternalIP","address":"good"}]}}]}' >"${fixtures}/nodes.1.json"
if output="$(run_script 2>&1)"; then
  fail 'case 26: a node with no metadata.uid must not be accepted'
fi
require_text "${output}" 'no metadata.uid' 'case 26: names the missing identity'

reset_node_sequence

# ===========================================================================
# Case 27 — ONE read per config file. The disabled verdict and bin_dir describe
# the same file, so they must come from the same view of it. Two reads could
# straddle a Talos or containerd update — the fleet workflow uses a different
# concurrency group from deployments, so it can overlap one — and a replacement
# that disables the plugin without removing its binaries would then be assembled
# into an OK verdict from two snapshots that never coexisted. The node UID does
# not change, so the convergence loop would never retry it.
#
# Asserted by COUNTING reads rather than by inspecting the code, so the property
# survives any future refactor of how the facts are extracted.
# ===========================================================================
write_node oneread
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/oneread/files/_etc_cri_conf.d_cri.toml"
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/oneread/files/_etc_containerd_config.toml"
write_dir oneread /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

rm -f "${fixtures}/read-log"
run_script TALOS_NODES=oneread >/dev/null 2>&1 ||
  fail 'case 27 control: the fixture must be a node that PASSES, or the count below proves nothing'

for probed in /etc/cri/conf.d/cri.toml /etc/containerd/config.toml; do
  reads="$(grep -c "^oneread ${probed}\$" "${fixtures}/read-log" || true)"
  [[ "${reads}" -eq 1 ]] ||
    fail "case 27: ${probed} was read ${reads} time(s), expected exactly 1 — the two facts can describe different snapshots"
done
printf 'ok — case 27: each config file is read exactly once\n' >/dev/null

reset_node_sequence

# ===========================================================================
# 28. A MULTILINE BASIC STRING'S BACKSLASH LINE CONTINUATION. (#3320 review)
#     TOML says a backslash immediately before a physical newline inside a
#     MULTILINE BASIC string removes the newline AND all leading whitespace of
#     the next line. So this entry resolves to exactly our plugin id and
#     containerd switches the verifier off.
#
#     The array is joined with a single space, and decode_basic() had no case
#     for `\` before that join point, so the buffered value kept the backslash
#     and compared unequal -- disabled=0, and the node reported OK with the
#     verifier disabled. That is a FAIL-OPEN on the one signal meant to mean
#     enforcement is on.
#
#     🔴 The heredoc MUST be quoted. Unquoted, bash removes the backslash-newline
#     while WRITING the fixture, collapsing it to the plain identifier -- which
#     the parser already handles, so the case would pass while testing nothing.
#     Measured: unquoted yields 1 line and no backslash, quoted yields 2.
# ===========================================================================
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["""io.containerd.image-verifier.v1.\
bindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
# The fixture must actually carry the continuation, or this case is vacuous.
grep -q '\\$' "${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" ||
  fail 'case 28: the fixture lost its backslash continuation — the case would pass vacuously'
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 28: a line-continued multiline basic string naming our plugin must fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 28: says WHY, so the operator edits the right setting'

# CONTROL 1 — MULTILINE LITERAL strings do NOT apply the continuation rule, so
# the same text is a DIFFERENT plugin id containerd never matches. Applying the
# continuation here would alias a foreign id onto ours and fail a node whose
# verifier is ENABLED — a false alarm on the signal that means enforcement is on.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ['''io.containerd.image-verifier.v1.\
bindir''']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
grep -q '\\$' "${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" ||
  fail 'case 28 control: the literal fixture lost its backslash — the control proves nothing'
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 28 control: a multiline LITERAL string does not continue lines, so that entry is a different plugin id and must not disable ours'
require_text "${output}" 'OK   disabled' 'case 28 control: literal strings keep the backslash verbatim'

# CONTROL 2 — a backslash-continued entry naming SOME OTHER plugin must still
# not disable ours. Without this, the fix could be satisfied by treating any
# continued entry as a match.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["""io.containerd.snapshotter.v1.\
blockfile"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 28 control: a continued entry naming another plugin must not disable ours'
require_text "${output}" 'OK   disabled' 'case 28 control: continuation is decoded, not treated as a wildcard match'

reset_node_sequence

# ===========================================================================
# 29. A CLOSING RUN OF FOUR OR FIVE QUOTES. (#3320 review)
#     TOML reads the FIRST quote of a four-quote run as content and the last
#     three as the delimiter, so this entry is a plugin id ending in a quote --
#     a DIFFERENT plugin, and our verifier stays ENABLED. Measured with
#     go-toml: """id"""" -> `id"` and """id""""" -> `id""`.
#
#     Closing at the first three quotes compared the bare id and reported the
#     node disabled, so a healthy node fails the gate.
# ===========================================================================
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["""io.containerd.image-verifier.v1.bindir""""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 29: a four-quote closing run leaves a trailing quote in the value, so that is a different plugin id and must not disable ours'
require_text "${output}" 'OK   disabled' 'case 29: the first quote of a four-quote run is content'

cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["""io.containerd.image-verifier.v1.bindir"""""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 29: a five-quote closing run leaves two trailing quotes, so that is a different plugin id and must not disable ours'
require_text "${output}" 'OK   disabled' 'case 29: the first two quotes of a five-quote run are content'

# CONTROL -- the plain three-quote close still names our plugin and must fail.
# Without this, the fix could be satisfied by never matching a multiline entry.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["""io.containerd.image-verifier.v1.bindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 29 control: a three-quote close names our plugin and must still fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 29 control: the ordinary multiline form is still matched'

reset_node_sequence

# ===========================================================================
# 30. A LEADING SPACE THAT IS CONTENT, NOT A JOINED LINE BREAK. (#3320 review)
#     TOML trims a newline that immediately follows the opening delimiter, and
#     the array's physical lines were joined with a single space -- so the
#     parser dropped one leading space to represent that trimmed newline.
#
#     But a space written on the SAME line is content: go-toml resolves
#     """ id""" to " id", a DIFFERENT plugin, so our verifier stays ENABLED.
#     Trimming it unconditionally reported a healthy node as disabled.
# ===========================================================================
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = [""" io.containerd.image-verifier.v1.bindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 30: a same-line leading space is content, so that entry is a different plugin id and must not disable ours'
require_text "${output}" 'OK   disabled' 'case 30: only a real line break after the opener is trimmed'

# CONTROL -- a real newline after the opener IS trimmed, so this names our
# plugin and must fail. This is the half the trim exists for; without it the
# fix could be satisfied by never trimming anything.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["""
io.containerd.image-verifier.v1.bindir"""]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 30 control: a newline immediately after the opener is trimmed, so this names our plugin and must fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 30 control: the trimmed-newline form is still matched'

reset_node_sequence

# ===========================================================================
# 31. A COMMA INSIDE A STRING IS CONTENT, NOT AN ENTRY SEPARATOR. (#3320 review)
#     Splitting the raw array text at every comma invents a second entry out of
#     one string. go-toml resolves
#       ['other, "io.containerd.image-verifier.v1.bindir"']
#     to a SINGLE string naming a different plugin, so our verifier stays
#     ENABLED -- but the split produced an apparent entry equal to our id and
#     reported the healthy node disabled.
# ===========================================================================
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ['other, "io.containerd.image-verifier.v1.bindir"']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 31: a comma inside a literal string is content, so that is one different plugin id and must not disable ours'
require_text "${output}" 'OK   disabled' 'case 31: commas are tokenized with quote state'

# CONTROL -- a GENUINE two-entry array whose second entry is ours must still
# fail. Without this, the fix could be satisfied by never splitting at all.
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ['io.containerd.snapshotter.v1.blockfile', 'io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 31 control: a real second entry naming our plugin must still fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 31 control: real separators are still split'

reset_node_sequence

# ===========================================================================
# 32. A CRLF PHYSICAL LINE BOUNDARY. (#3320 review)
#     A config assembled from fragments with mixed line endings can carry CRLF.
#     TOML removes that line ending exactly as it removes LF, so both of these
#     resolve to our plugin id and containerd switches the verifier OFF
#     (measured with go-toml).
#
#     awk splits records on newline only, so each line kept a trailing CR; the
#     continuation rule recognised space and tab alone, the CR survived into the
#     buffered value, the comparison failed, and the script printed disabled=0
#     -- a node reporting OK with no enforcement. That is the FAIL-OPEN this
#     script exists to catch.
#
#     printf writes the CR: a heredoc cannot carry one portably.
# ===========================================================================
printf 'version = 3\ndisabled_plugins = ["""io.containerd.image-verifier.v1.\\\r\nbindir"""]\n\n[plugins]\n  [plugins.%s]\n    bin_dir = %s\n' \
  "'io.containerd.image-verifier.v1.bindir'" "'/opt/containerd/image-verifier/bin'" \
  >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml"
# The fixture must actually carry CR before the newline, or the case is vacuous.
grep -q "$(printf '\\\r$')" "${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" ||
  fail 'case 32: the fixture lost its CRLF continuation — the case would pass vacuously'
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 32: a CRLF line continuation naming our plugin must fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 32: CRLF boundaries are decoded like LF'

# The same hole at the opener: a CRLF immediately after """ is trimmed too.
printf 'version = 3\ndisabled_plugins = ["""\r\nio.containerd.image-verifier.v1.bindir"""]\n\n[plugins]\n  [plugins.%s]\n    bin_dir = %s\n' \
  "'io.containerd.image-verifier.v1.bindir'" "'/opt/containerd/image-verifier/bin'" \
  >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml"
grep -q "$(printf '"""\r$')" "${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" ||
  fail 'case 32: the opener fixture lost its CR — the case would pass vacuously'
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 32: a CRLF immediately after the opening delimiter is trimmed, so this names our plugin and must fail the node'
fi
require_text "${output}" 'disabled_plugins' 'case 32: a CRLF after the opener is trimmed like an LF'

# The same CRLF boundary on a CONTINUATION line, not the array's first line.
# The two are stripped by different rules -- the opening line is consumed by the
# key rule, every later line by the array rule -- so a fixture that only ever puts
# the CR on the first line leaves the second rule untested.
printf 'version = 3\ndisabled_plugins = [\r\n"""io.containerd.image-verifier.v1.\\\r\nbindir"""]\r\n\n[plugins]\n  [plugins.%s]\n    bin_dir = %s\n' "'io.containerd.image-verifier.v1.bindir'" "'/opt/containerd/image-verifier/bin'" >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml"
grep -q "$(printf '\\\r$')" "${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" ||
  fail 'case 32: the continuation-line fixture lost its CRLF -- the case would pass vacuously'
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 32: a CRLF continuation on a later array line must fail the node too'
fi
require_text "${output}" 'disabled_plugins' 'case 32: CRLF is stripped on every array line, not just the first'

# CONTROL -- a CR that is NOT at a line boundary is ordinary content, so this
# names a different plugin and must not disable ours.
printf 'version = 3\ndisabled_plugins = ["""io.containerd.image-verifier.v1.\\rbindir"""]\n\n[plugins]\n  [plugins.%s]\n    bin_dir = %s\n' \
  "'io.containerd.image-verifier.v1.bindir'" "'/opt/containerd/image-verifier/bin'" \
  >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml"
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 32 control: an escaped \r inside the value is content, so that is a different plugin id and must not disable ours'
require_text "${output}" 'OK   disabled' 'case 32 control: only a CR at a physical line boundary is a line ending'

reset_node_sequence

# ===========================================================================
# 33. THE CONVERGENCE OVERRIDE IS A DECIMAL INTEGER. (#3320 review)
#     The value passes the digit-only filter, then reaches bash arithmetic,
#     which applies base-prefix rules to a leading zero. Measured in bash:
#     `[[ 08 -ge 1 ]]` errors with "value too great for base", and 010 is
#     EIGHT. So a documented, digit-only override is either rejected outright
#     or silently runs a different number of convergence attempts than asked.
# ===========================================================================
write_node zeropad
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/zeropad/files/_etc_cri_conf.d_cri.toml"
write_dir zeropad /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   drwxr-xr-x   0     0     37        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   .
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

output="$(run_script TALOS_NODES=zeropad IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS=08 2>&1)" ||
  fail 'case 33: a zero-padded attempt count is a positive integer and must be accepted'
require_text "${output}" 'OK   zeropad' 'case 33: 08 is read as decimal 8'

# CONTROL -- the guard still rejects a non-integer. Without this, the fix could
# be satisfied by dropping the validation altogether.
if output="$(run_script TALOS_NODES=zeropad IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS=2x 2>&1)"; then
  fail 'case 33 control: a non-integer attempt count must still be rejected'
fi
require_text "${output}" 'must be a positive integer' 'case 33 control: the digit filter still applies'

# CONTROL -- zero is still not a positive integer, however it is padded.
if output="$(run_script TALOS_NODES=zeropad IMAGE_VERIFIER_CONVERGENCE_ATTEMPTS=00 2>&1)"; then
  fail 'case 33 control: a padded zero is still not at least 1'
fi
require_text "${output}" 'at least 1' 'case 33 control: 00 is decimal zero, still rejected'

reset_node_sequence

# ===========================================================================
# 34. THE CLOSING-RUN RULE IS NEEDED IN THE ARRAY SCANNER TOO. (#3320 review)
#     Case 29 fixed the run in the ENTRY comparison. The scanner that decides
#     where the array ENDS closes on the first three quotes as well, so it reads
#     the fourth as a new opener and swallows whatever follows as string content.
#
#     TOML lets a sub-table be written without its implicit parent, so the very
#     next line can be the verifier's own table. Swallowed, its populated bin_dir
#     is never seen and a HEALTHY node fails the gate. The case 29 fixtures all
#     carry an explicit [plugins] header, which absorbs the mis-parse and hides
#     this -- so this case deliberately omits it.
# ===========================================================================
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["""io.containerd.snapshotter.v1.blockfile""""]

[plugins.'io.containerd.image-verifier.v1.bindir']
  bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 34: a four-quote close must not swallow the verifier table that follows it'
require_text "${output}" 'OK   disabled' 'case 34: the array scanner consumes the whole closing quote run'

reset_node_sequence

# ===========================================================================
# 35. A `[` INSIDE A TOP-LEVEL MULTILINE STRING IS NOT A TABLE HEADER. (#3320 review)
#     The header rule fires on any line starting with `[` and clears root scope
#     permanently. A physical line of an earlier top-level multiline string that
#     begins with `[` therefore ends root scope, and the REAL disabled_plugins
#     below it is never examined -- disabled stays 0, the verifier table still
#     supplies a populated bin_dir, and the script reports OK while containerd
#     has the plugin switched off. That is the FAIL-OPEN direction.
# ===========================================================================
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
note = """
[this is string content, not a table header]
"""
disabled_plugins = ['io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
if output="$(run_script TALOS_NODES=disabled 2>&1)"; then
  fail 'case 35: a bracketed line inside a multiline string must not end root scope and hide the real disabled_plugins'
fi
require_text "${output}" 'disabled_plugins' 'case 35: the real root key is still read after a bracketed string line'

# CONTROL -- a REAL table header must still end root scope, or the fix would
# re-alias a non-root disabled_plugins onto the root one (the hole case 24 closed).
cat >"${fixtures}/disabled/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3

[some.other.table]
disabled_plugins = ['io.containerd.image-verifier.v1.bindir']

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
output="$(run_script TALOS_NODES=disabled 2>&1)" ||
  fail 'case 35 control: a non-root disabled_plugins must not be treated as the root one'
require_text "${output}" 'OK   disabled' 'case 35 control: a real table header still ends root scope'

reset_node_sequence

# ===========================================================================
# case 36 -- A NON-ASCII \U ESCAPE MUST NOT ALIAS ONTO THE VERIFIER ID.
#   `%c` is byte-oriented in several awks, so a code point above 255 is
#   truncated modulo 256: U+10069 emits the single byte 0x69 (`i`), so the
#   basic string below decoded to exactly `io.containerd.image-verifier.v1.bindir`.
#   This node disables a DIFFERENT plugin and verifies images perfectly well,
#   but the aliased decode reported it as having the verifier switched off --
#   a healthy node failed on a config that never names the verifier.
# ===========================================================================
write_node escaped
cat >"${fixtures}/escaped/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["\U00010069o.containerd.image-verifier.v1.bindir"]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
write_dir escaped /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   drwxr-xr-x   0     0     37        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   .
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF
output="$(run_script TALOS_NODES=escaped 2>&1)" ||
  fail 'case 36: a non-ASCII \U escape must not alias onto the verifier ID and disable a healthy node'
require_text "${output}" 'OK   escaped' 'case 36: the node that disables a different plugin still enforces'

# CONTROL -- an ASCII \u escape that really does spell the verifier ID must STILL
# be decoded and honoured. Without this the fix could be satisfied by decoding no
# escapes at all, which would hide a genuinely disabled verifier -- the FAIL-OPEN
# direction and far worse than the false alarm above.
write_node asciiesc
cat >"${fixtures}/asciiesc/files/_etc_cri_conf.d_cri.toml" <<'EOF'
version = 3
disabled_plugins = ["\u0069o.containerd.image-verifier.v1.bindir"]

[plugins]
  [plugins.'io.containerd.image-verifier.v1.bindir']
    bin_dir = '/opt/containerd/image-verifier/bin'
EOF
write_dir asciiesc /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   drwxr-xr-x   0     0     37        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   .
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF
if output="$(run_script TALOS_NODES=asciiesc 2>&1)"; then
  fail 'case 36 control: an ASCII \u escape spelling the verifier ID must still be decoded and honoured'
fi
require_text "${output}" 'disabled_plugins' 'case 36 control: the genuinely disabled verifier is still detected'

# ===========================================================================
# Case 37 — a discovered node the autoscaler REMOVES mid-pass is an ordinary
# node-set change, not an infrastructure failure. (#3320 review)
#
# `path_probe` fails for a node that has left, and that reachability failure used
# to exit 2 immediately, so the post-pass inventory read never got the chance to
# notice the node-set change and retry. The independently-concurrent
# validate-image-verifier-liveness.yaml workflow therefore failed during an
# ordinary scale-down or replacement, despite the convergence loop existing.
# ===========================================================================
install_sequenced_kubectl
reset_node_sequence

write_node departing
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/departing/files/_etc_cri_conf.d_cri.toml"
# It answers the config read and then stops answering, which is exactly how a node
# being drained behaves: the pass gets past the config and only then finds it gone.
printf 'DEPARTED\n' >"${fixtures}/departing/lsfail_all"

# Call 1 sees both; every later read sees only the survivor, so the node really has
# left rather than merely gone quiet.
printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-2 uid-2 departing)" \
  >"${fixtures}/nodes.1.json"
for call in 2 3 4; do
  printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.${call}.json"
done

status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 0 ]] ||
  fail "case 37: a node that left mid-pass must converge and report, got exit ${status} — ${output}"
require_text "${output}" 'The node set changed while it was being checked' 'case 37: says it re-ran'
require_text "${output}" 'All 1 node(s)' 'case 37: the verdict counts the fleet that actually exists'
refute_text "${output}" 'cannot reach node departing' 'case 37: blamed infrastructure for an ordinary scale-down'

# ===========================================================================
# Case 37b CONTROL — a node that is unreachable but STILL IN THE FLEET must
# still fail as infrastructure. (#3320 review)
#
# Without this, case 37 would be satisfied by any change that turns every
# unreachable node into a retry — downgrading a real fault into a silent re-run
# and eventually into a verdict for a fleet that was never checked, which is the
# fail-open this whole script exists to detect.
# ===========================================================================
install_sequenced_kubectl
reset_node_sequence

write_node stuck
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/stuck/files/_etc_cri_conf.d_cri.toml"
printf 'STUCK\n' >"${fixtures}/stuck/lsfail_all"

# The fleet never changes: `stuck` is still registered, it just stopped answering.
for call in 1 2 3 4; do
  printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-3 uid-3 stuck)" \
    >"${fixtures}/nodes.${call}.json"
done

status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] ||
  fail "case 37b: an unreachable node still in the fleet must exit 2 (infrastructure), got ${status} — ${output}"
require_text "${output}" 'cannot reach node stuck' 'case 37b: names the unreachable node'
refute_text "${output}" 'can enforce image verification.' 'case 37b: reported a verdict it could not reach'

# ===========================================================================
# Case 37c/d/e — EVERY departure path is guarded independently. (#3320 review)
#
# Case 37 reaches only the `directory_exists` failure. The departure handling also
# sits on the `path_probe` absent arm, the `path_probe` error arm, and the
# executable count — and with one shared case a later change could drop
# `node_departed` from any of the three and still pass 37 and 37b. Each path gets
# its own fixture, so each is a regression case on its own.
#
# `lsfail_root` rather than `lsfail_all` on the two path_probe arms: those arms are
# only reached when the file probe RETURNED something, so the node has to answer the
# probe and then stop answering the reachability check.
# ===========================================================================

# 37c — the `absent` arm: the node has no such config, then it is gone.
install_sequenced_kubectl
reset_node_sequence
write_node gone-absent
printf 'GONE\n' >"${fixtures}/gone-absent/lsfail_root"
printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-4 uid-4 gone-absent)" \
  >"${fixtures}/nodes.1.json"
for call in 2 3 4; do
  printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.${call}.json"
done
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 0 ]] ||
  fail "case 37c: a node that left on the path_probe ABSENT arm must converge, got exit ${status} — ${output}"
require_text "${output}" 'All 1 node(s)' 'case 37c: reports the settled fleet'

# 37d — the `error` arm: the probe fails for a reason other than absence, then gone.
install_sequenced_kubectl
reset_node_sequence
write_node gone-error
printf 'GONE\n' >"${fixtures}/gone-error/lsfail_root"
printf 'RPC\n' >"${fixtures}/gone-error/lserror_etc_cri_conf.d_cri.toml"
printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-5 uid-5 gone-error)" \
  >"${fixtures}/nodes.1.json"
for call in 2 3 4; do
  printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.${call}.json"
done
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 0 ]] ||
  fail "case 37d: a node that left on the path_probe ERROR arm must converge, got exit ${status} — ${output}"
require_text "${output}" 'All 1 node(s)' 'case 37d: reports the settled fleet'

# 37e — the executable count: bin_dir exists, the long listing fails, then gone.
install_sequenced_kubectl
reset_node_sequence
write_node gone-count
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/gone-count/files/_etc_cri_conf.d_cri.toml"
write_dir gone-count /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   drwxr-xr-x   0     0     37        Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   .
EOF
# `lsfail` fails only the LONG form, so directory_exists passes and the count fails.
printf 'GONE\n' >"${fixtures}/gone-count/lsfail"
printf '{"items":[%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" "$(node_json prod-worker-6 uid-6 gone-count)" \
  >"${fixtures}/nodes.1.json"
for call in 2 3 4; do
  printf '{"items":[%s]}' "$(node_json prod-worker-1 uid-1 good)" >"${fixtures}/nodes.${call}.json"
done
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 0 ]] ||
  fail "case 37e: a node that left on the executable COUNT must converge, got exit ${status} — ${output}"
require_text "${output}" 'All 1 node(s)' 'case 37e: reports the settled fleet'

# CONTROLS — each of the three paths must STILL fail infra when the node is present.
# Without these, 37c/d/e would be satisfied by turning every fault into a retry.
install_sequenced_kubectl
reset_node_sequence
for call in 1 2 3 4; do
  printf '{"items":[%s,%s,%s,%s]}' "$(node_json prod-worker-1 uid-1 good)" \
    "$(node_json prod-worker-4 uid-4 gone-absent)" "$(node_json prod-worker-5 uid-5 gone-error)" \
    "$(node_json prod-worker-6 uid-6 gone-count)" >"${fixtures}/nodes.${call}.json"
done
status=0
output="$(run_script 2>&1)" || status=$?
[[ "${status}" -eq 2 ]] ||
  fail "case 37f: unreachable nodes still in the fleet must exit 2, got ${status} — ${output}"
refute_text "${output}" 'can enforce image verification.' 'case 37f: reported a verdict it could not reach'

reset_node_sequence

printf 'PASS: all cases\n'
