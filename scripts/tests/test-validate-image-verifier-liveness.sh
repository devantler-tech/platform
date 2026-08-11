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
# Fake talosctl: serves `read <file>` and `ls [-l] <dir>` from a fixture tree.
# A path with no fixture behaves like the real thing — non-zero, nothing on
# stdout — which is what makes the "directory does not exist" case real rather
# than simulated by a special-case flag.
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
      [[ "${1:-}" == '-l' ]] && shift || true
      path="$1"
      listing="${FIXTURES}/${node}/dirs/${path//\//_}"
      [[ -f "${listing}" ]] || { printf 'lstat %s: no such file or directory\n' "${path}" >&2; exit 1; }
      cat "${listing}"
      exit 0
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
require_text "${output}" 'no bin_dir configured' 'case 3: names the condition that failed'

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
# Case 8 — the SYSTEM containerd config counts too. A verifier wired only into
# the CRI containerd leaves Talos' own image pulls unverified, so a bin_dir
# found in either file satisfies the check.
# ===========================================================================
write_node systemonly
no_verifier_config >"${fixtures}/systemonly/files/_etc_cri_conf.d_cri.toml"
verifier_config /opt/containerd/image-verifier/bin >"${fixtures}/systemonly/files/_etc_containerd_config.toml"
write_dir systemonly /opt/containerd/image-verifier/bin <<EOF
${listing_header}
10.0.1.4   -rwxr-xr-x   0     0     8123456   Aug  4 14:58:45   system_u:object_r:containerd_plugin_t:s0   cosign-verifier
EOF

output="$(run_script TALOS_NODES=systemonly 2>&1)" ||
  fail 'case 8: a bin_dir declared only in the system containerd config must still count'

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
require_text "${output}" 'no bin_dir configured' 'case 12: reached the real verdict'

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

printf 'PASS: all cases\n'
