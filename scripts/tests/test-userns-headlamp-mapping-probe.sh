#!/usr/bin/env bash
# Behaviour and wiring tests for the user-namespace mapping probe Job.
#
# The probe measures the subordinate UID/GID mapping a user-namespaced pod
# receives in the `headlamp` namespace (#2651, acceptance criterion 5). It is
# applied into the `apps` Flux Kustomization, which runs `wait: true` with no
# explicit `healthChecks` — so kstatus evaluates every applied resource and a
# Job reporting `Failed` takes the whole apps layer not-Ready.
#
# That is why the probe reports its result as a VERDICT LINE and always exits 0:
# its own designed negative result must not be able to wedge the layer it is
# measuring. The verdict is therefore what these tests pin, and "the container
# exits 0" is itself the property under test in every behaviour case.
#
# The script under test is extracted from the shipped manifest rather than
# copied here, so the tests cannot pass against a script that is not the one
# deployed. It reads its id-map paths from override variables purely as a test
# seam; the wiring half asserts the manifest sets no `env`, so that seam cannot
# be exercised in production.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly job="${root_dir}/k8s/providers/hetzner/apps/userns-headlamp-mapping-probe/job.yaml"
readonly parent="${root_dir}/k8s/providers/hetzner/apps/kustomization.yaml"

pass_count=0
fail_count=0

ok() {
  echo "  ✅ $1"
  pass_count=$((pass_count + 1))
}

bad() {
  echo "  ❌ $1"
  echo "     $2"
  fail_count=$((fail_count + 1))
}

if [ ! -f "${job}" ]; then
  echo "::error::probe manifest not found at ${job}"
  exit 1
fi

# Extract the shipped script. Fail closed rather than test an empty string: an
# empty extraction would make every behaviour assertion below vacuous.
script="$(yq -r '.spec.template.spec.containers[] | select(.name == "read-id-maps") | .args[0]' "${job}")"
if [ -z "${script}" ] || [ "${script}" = "null" ]; then
  echo "::error::could not extract the read-id-maps script from ${job}"
  exit 1
fi

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

# Take the INVOCATION from the manifest too, not just the script. The container
# runs `/bin/sh -c <script>`, and the absence of `-e` is the property under test:
# under `-e` a field assignment whose command substitution fails would abort the
# script before any verdict was printed. Reading the invocation from the manifest
# is what makes a future `-ec` a test failure rather than a silent regression —
# a test that ran plain `sh` would pass either way.
# Read with a loop rather than `mapfile`, which is bash 4+ and absent on the
# bash 3.2 shipped with macOS — the suite must be runnable where it is authored,
# not only on the CI runner.
probe_command=()
while IFS= read -r arg; do
  probe_command+=("${arg}")
done < <(
  yq -r '.spec.template.spec.containers[] | select(.name == "read-id-maps") | .command[]' "${job}"
)
if [ "${#probe_command[@]}" -eq 0 ]; then
  echo "::error::could not extract the read-id-maps command from ${job}"
  exit 1
fi

# run_probe UID_MAP_CONTENT GID_MAP_CONTENT -> prints "<exit_code>\n<stdout>"
#
# Runs the shipped script through the shipped command, with only the id-map
# paths redirected at fixtures.
run_probe() {
  local uid_content="$1" gid_content="$2"
  local dir
  dir="$(mktemp -d "${workdir}/case.XXXXXX")"
  printf '%s' "${uid_content}" >"${dir}/uid_map"
  printf '%s' "${gid_content}" >"${dir}/gid_map"

  local out rc
  set +e
  out="$(USERNS_PROBE_UID_MAP="${dir}/uid_map" USERNS_PROBE_GID_MAP="${dir}/gid_map" \
    "${probe_command[@]}" "${script}" 2>&1)"
  rc=$?
  set -e
  printf '%s\n' "${rc}"
  printf '%s\n' "${out}"
}

# assert_case NAME UID_MAP GID_MAP EXPECTED_VERDICT
assert_case() {
  local name="$1" uid_map="$2" gid_map="$3" expected="$4"
  local result rc out
  result="$(run_probe "${uid_map}" "${gid_map}")"
  rc="$(printf '%s\n' "${result}" | head -1)"
  out="$(printf '%s\n' "${result}" | tail -n +2)"

  # The wedge-proof property: whatever the reading, the container exits 0 so a
  # negative result cannot take the apps layer not-Ready.
  if [ "${rc}" != "0" ]; then
    bad "${name}: exits 0" "exit code was ${rc}; a non-zero exit wedges the apps layer"
    return
  fi
  ok "${name}: exits 0"

  if printf '%s\n' "${out}" | grep -qxF "USERNS-PROBE-VERDICT=${expected}" 2>/dev/null ||
    printf '%s\n' "${out}" | grep -qF "USERNS-PROBE-VERDICT=${expected} " 2>/dev/null; then
    ok "${name}: reports ${expected}"
  else
    bad "${name}: reports ${expected}" "verdict line missing; output was: ${out}"
  fi
}

echo "▶ Behaviour: the verdict is carried by a log line, not by the exit status"

# A real user namespace: the container range starts at 0 and maps onto a
# non-zero host range of bounded width.
assert_case "non-identity mapping" \
  "0 100000 65536" "0 100000 65536" "NON-IDENTITY"

# The failure this probe exists to detect: the pod was left in the host user
# namespace, which reports the full-width identity map.
assert_case "identity map (hostUsers silently ignored)" \
  "0 0 4294967295" "0 0 4294967295" "IDENTITY-MAP"

# A non-zero-based full-width map would clear a host-start check on its own, so
# the width is asserted separately and must still be caught.
assert_case "non-zero-based full-width map" \
  "0 1 4294967295" "0 1 4294967295" "IDENTITY-MAP"

# A GID map that disagrees with a good UID map must not be reported as a pass.
assert_case "uid mapped, gid left in the host namespace" \
  "0 100000 65536" "0 0 4294967295" "IDENTITY-MAP"

# The case above is caught by the WIDTH check, so on its own it leaves the
# gid host-start comparison untested — ablating that conjunct flipped nothing.
# A bounded gid map based at host 0 is not a subordinate range either, and only
# the host-start comparison rejects it.
assert_case "gid range bounded but based at host 0" \
  "0 100000 65536" "0 0 65536" "IDENTITY-MAP"

# The mirror of the case above, so the uid host-start comparison is likewise
# pinned by an arm that the width check cannot also catch.
assert_case "uid range bounded but based at host 0" \
  "0 0 65536" "0 100000 65536" "IDENTITY-MAP"

# A zero-LENGTH range maps nothing at all, so it is not a subordinate range
# either — but it clears both a `host_start > 0` test and an upper-bound width
# test, so nothing rejected it and it read as a pass. One arm per side: a check
# that only bounded the uid length would still accept a zero-length gid range.
assert_case "uid range is zero-length" \
  "0 100000 0" "0 100000 65536" "IDENTITY-MAP"

assert_case "gid range is zero-length" \
  "0 100000 65536" "0 100000 0" "IDENTITY-MAP"

echo "▶ Behaviour: an unreadable reading is INDETERMINATE, never a pass"

# An empty or unreadable id-map must not be silently treated as either verdict.
assert_case "empty id-map" "" "" "INDETERMINATE"

# A non-numeric field must be rejected BEFORE any arithmetic test. `[ x -gt 0 ]`
# on a non-integer errors, and an errored test inside an `if` fails OPEN — which
# would report a pass for a reading that was never understood.
assert_case "non-numeric host-start" \
  "0 abc 65536" "0 100000 65536" "INDETERMINATE"

assert_case "non-numeric length" \
  "0 100000 wide" "0 100000 65536" "INDETERMINATE"

# The columns the comparison does NOT read still decide whether the record is
# the format this probe knows. Both arms below carry a perfectly valid
# host-start and length, so every numeric check passes and only the shape check
# can reject them — which is exactly why they classified as NON-IDENTITY before.
assert_case "non-numeric container-start" \
  "x 100000 65536" "0 100000 65536" "INDETERMINATE"

assert_case "extra field on the record" \
  "0 100000 65536 extra" "0 100000 65536" "INDETERMINATE"

# The same shape defect on the gid side: a check that only validated the uid map
# would pass this and read a gid record it never understood.
assert_case "extra field on the gid record" \
  "0 100000 65536" "0 100000 65536 extra" "INDETERMINATE"

echo "▶ Wiring: nothing re-arms errexit, which would restore the wedge"

# `-e` is the one flag that can defeat the always-exit-0 guarantee without any
# bad reading: under it, a field assignment whose command substitution fails
# (`x="$(… | awk …)"`) aborts with that command's status before a verdict is
# ever printed — a Failed Job, and the layer not-Ready. The guarantee has to be
# structural, not a promise in a comment, so assert on both places it can enter.
# Herestring, not a pipe, for the same reason as the check below: `grep -q` plus
# `set -o pipefail` reports a MATCH as a failed pipeline. This input is short
# enough that printf usually finishes before grep exits, so the bug is latent
# here rather than active — which is exactly why it must not be left in place.
if grep -qE -- '^-[a-z]*e' <<<"$(printf '%s\n' "${probe_command[@]}")"; then
  bad "the shipped command does not enable errexit" \
    "a command flag carries -e; a failing command substitution would then exit non-zero before any verdict"
else
  ok "the shipped command does not enable errexit"
fi

# BOTH spellings: `set -e` (and its letter-cluster forms) and the long
# `set -o errexit`, which is exactly equivalent and which a short-form-only
# pattern does not match.
#
# 🔴 FED BY A HERESTRING, NOT A PIPE, AND THAT IS THE WHOLE CHECK. This file runs
# under `set -o pipefail`. In `printf ... | grep -q PATTERN`, grep exits the
# instant it matches, so printf is left writing into a closed pipe and dies of
# SIGPIPE (141). pipefail then reports the PIPELINE as failed — on the match —
# so the `if` took the else branch and this guard reported ok for a script that
# did re-enable errexit. It reported ok for a non-matching script too, because
# grep exits 1 there. It could not fail in either direction.
#
# The race needs enough input that printf is still writing when grep exits: a
# two-line probe passes, the real ~3.9 KB script does not. That is why it has to
# be verified against the shipped script rather than a fixture.
if grep -qE '^[[:space:]]*set[[:space:]]+(-[a-z]*e|-o[[:space:]]+errexit)' <<<"${script}"; then
  bad "the script does not re-enable errexit" \
    "found a 'set -e' or 'set -o errexit' in the probe script"
else
  ok "the script does not re-enable errexit"
fi

# Behavioural proof of the same property, driven through the shipped command:
# a failing command substitution must not stop the verdict being reported.
# shellcheck disable=SC2016  # deliberately literal: the PROBE's shell must
# evaluate this substitution, not this one — expanding it here would test nothing.
errexit_snippet='x="$(exit 3)"
printf "REACHED-END\n"'
errexit_out="$("${probe_command[@]}" "${errexit_snippet}" 2>&1 || true)"
if printf '%s\n' "${errexit_out}" | grep -qxF 'REACHED-END'; then
  ok "a failing command substitution does not abort under the shipped command"
else
  bad "a failing command substitution does not abort under the shipped command" \
    "the shipped command aborted early, so a verdict would never be printed"
fi

echo "▶ Wiring: the production Job cannot reach the test seam"

# The override variables exist so these tests can drive the shipped script. If
# the manifest ever set them, the probe would measure a file of someone else's
# choosing instead of its own user namespace.
env_block="$(yq -r '.spec.template.spec.containers[] | select(.name == "read-id-maps") | .env // "none"' "${job}")"
if [ "${env_block}" = "none" ]; then
  ok "probe container declares no env, so the id-map paths cannot be overridden in-cluster"
else
  bad "probe container declares no env" "found env on the probe container: ${env_block}"
fi

# `env:` is not the only way in. envFrom injects every key of a ConfigMap or
# Secret into the container, so an envFrom source could define
# USERNS_PROBE_UID_MAP without `env:` ever appearing — the check above would
# still report ok. The source's contents live in another object this test cannot
# resolve, so any envFrom at all is refused rather than inspected.
env_from="$(yq -r '.spec.template.spec.containers[] | select(.name == "read-id-maps") | .envFrom // "none"' "${job}")"
if [ "${env_from}" = "none" ]; then
  ok "probe container declares no envFrom, so no external source can define the override keys"
else
  bad "probe container declares no envFrom" "found envFrom on the probe container: ${env_from}"
fi

# The probe is a disposable diagnostic: it must stay out of the parent
# kustomization until a deliberate activation PR.
#
# Fail closed on the file itself first. `grep` exits 2 for an unreadable or
# missing ${parent} and 1 for no match, and BOTH land in the else branch — so a
# renamed or deleted kustomization would report "staged" without anything having
# been checked. The pattern also normalises the optional `./` prefix, which
# kustomize accepts and the previous pattern did not match: an active component
# written `- ./userns-headlamp-mapping-probe/` read as absent.
if [ ! -r "${parent}" ]; then
  bad "probe stays staged (commented out) in the apps kustomization" \
    "cannot read ${parent}, so the component's activation state was never established"
elif grep -qE '^[[:space:]]*-[[:space:]]+\.?/?userns-headlamp-mapping-probe/?[[:space:]]*$' "${parent}"; then
  bad "probe stays staged (commented out) in the apps kustomization" \
    "the component is active in ${parent}; it must be activated only by a short-lived PR and removed after (#2858)"
else
  ok "probe stays staged (commented out) in the apps kustomization"
fi

# backoffLimit 0 keeps a single reading rather than retrying a measurement.
backoff="$(yq -r '.spec.backoffLimit' "${job}")"
if [ "${backoff}" = "0" ]; then
  ok "backoffLimit is 0, so the log holds exactly one reading"
else
  bad "backoffLimit is 0" "was ${backoff}"
fi

echo
echo "passed: ${pass_count}  failed: ${fail_count}"
[ "${fail_count}" -eq 0 ]
