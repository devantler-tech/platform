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
# runs `/bin/sh -ec <script>`, and `-e` stays in force regardless of what the
# script's own `set` line says — so a test that ran plain `sh` would not be
# exercising the shipped configuration, and a future command that fails under
# `-e` would abort the container while the test still passed.
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

# The probe is a disposable diagnostic: it must stay out of the parent
# kustomization until a deliberate activation PR.
if grep -qE '^[[:space:]]*-[[:space:]]+userns-headlamp-mapping-probe/' "${parent}"; then
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
