#!/usr/bin/env bash
set -euo pipefail

# The real script pins one version and one digest, so its constants cannot match
# a fixture. Each case runs the SHIPPED logic with those two constants
# substituted, and asserts the substitution actually fired — a silently failed
# rewrite would otherwise leave every case exercising the production pin against
# a fixture and "passing" for the wrong reason.

repo_root=$(git rev-parse --show-toplevel)
script_under_test="${repo_root}/.github/scripts/setup-talosctl.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/test-setup-talosctl.XXXXXX")
trap 'rm -rf "${test_root}"' EXIT

fixture_dir="${test_root}/fixtures"
fake_bin="${test_root}/bin"
runner_temp="${test_root}/runner"
mkdir -p "${fixture_dir}" "${fake_bin}" "${runner_temp}"

test_version='9.9.9'

printf '#!/usr/bin/env bash\nexit 0\n' >"${fixture_dir}/talosctl"
served_digest=$(sha256sum "${fixture_dir}/talosctl" | cut -d' ' -f1)

# A digest that is syntactically valid and belongs to nothing.
absent_digest=$(printf '%064d' 0)

cat >"${fake_bin}/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
output=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "${url}" in
  */talosctl-linux-amd64)
    cp "${TEST_FIXTURE_DIR}/talosctl" "${output}"
    ;;
  */sha256sum.txt)
    if [ "${TEST_MANIFEST_REACHABLE}" != '1' ]; then
      exit 22
    fi
    if [ -n "${output}" ]; then
      cp "${TEST_FIXTURE_DIR}/sha256sum.txt" "${output}"
    else
      cat "${TEST_FIXTURE_DIR}/sha256sum.txt"
    fi
    ;;
  *)
    printf 'unexpected URL: %s\n' "${url}" >&2
    exit 2
    ;;
esac
FAKE_CURL

cat >"${fake_bin}/sudo" <<'FAKE_SUDO'
#!/usr/bin/env bash
set -euo pipefail
touch "${TEST_INSTALL_MARKER}"
FAKE_SUDO

cat >"${fake_bin}/talosctl" <<'FAKE_TALOSCTL'
#!/usr/bin/env bash
exit 0
FAKE_TALOSCTL

chmod +x "${fake_bin}/curl" "${fake_bin}/sudo" "${fake_bin}/talosctl"

# Build a copy of the shipped script with the two pinned constants replaced.
make_variant() {
  pinned_digest=$1
  variant="${test_root}/setup-talosctl-variant.sh"
  sed \
    -e "s|^TALOS_VERSION=.*|TALOS_VERSION=\"${test_version}\"|" \
    -e "s|^TALOSCTL_SHA256=.*|TALOSCTL_SHA256=\"${pinned_digest}\"|" \
    "${script_under_test}" >"${variant}"
  chmod +x "${variant}"
  # Assert the substitution fired in BOTH places, or every case below is vacuous.
  grep -qx "TALOS_VERSION=\"${test_version}\"" "${variant}" ||
    { printf 'version substitution did not fire\n' >&2; exit 1; }
  grep -qx "TALOSCTL_SHA256=\"${pinned_digest}\"" "${variant}" ||
    { printf 'digest substitution did not fire\n' >&2; exit 1; }
}

run_installer() {
  rm -f "${test_root}/installed"
  if output=$(
    cd "${repo_root}" &&
      PATH="${fake_bin}:${PATH}" \
        RUNNER_TEMP="${runner_temp}" \
        TEST_FIXTURE_DIR="${fixture_dir}" \
        TEST_INSTALL_MARKER="${test_root}/installed" \
        TEST_MANIFEST_REACHABLE="${manifest_reachable:-1}" \
        "${test_root}/setup-talosctl-variant.sh" 2>&1
  ); then
    status=0
  else
    status=$?
  fi
}

assert_installed() {
  if [ "${status}" -ne 0 ]; then
    printf 'expected install to succeed, got status %s:\n%s\n' "${status}" "${output}" >&2
    exit 1
  fi
  if [ ! -e "${test_root}/installed" ]; then
    printf 'expected the binary to be installed, but sudo install never ran\n' >&2
    exit 1
  fi
}

assert_rejected_before_install() {
  expected_output=$1
  if [ "${status}" -eq 0 ]; then
    printf 'expected setup-talosctl.sh to reject the download\n%s\n' "${output}" >&2
    exit 1
  fi
  # The exit status alone does not prove the guard held — assert the side effect.
  if [ -e "${test_root}/installed" ]; then
    printf 'setup-talosctl.sh installed the binary despite failing verification\n' >&2
    exit 1
  fi
  case "${output}" in
    *"${expected_output}"*) ;;
    *)
      printf 'unexpected failure output: %s\n' "${output}" >&2
      exit 1
      ;;
  esac
}

# --- Case 1: the served bytes match the pin -> installs.
printf '%s  talosctl-linux-amd64\n' "${served_digest}" >"${fixture_dir}/sha256sum.txt"
make_variant "${served_digest}"
run_installer
assert_installed

# --- Case 2: pin stale (the Renovate failure) -> names the digest to write.
# The served bytes match the release's published manifest; only the pin is behind.
printf '%s  talosctl-linux-amd64\n' "${served_digest}" >"${fixture_dir}/sha256sum.txt"
make_variant "${absent_digest}"
run_installer
assert_rejected_before_install "the pinned talosctl digest is stale for v${test_version}"
case "${output}" in
  *"${served_digest}"*) ;;
  *)
    printf 'stale-pin error did not name the digest to write: %s\n' "${output}" >&2
    exit 1
    ;;
esac

# --- Case 3: the bytes match neither the pin nor the manifest -> substitution.
printf '%s  talosctl-linux-amd64\n' "$(printf '%064d' 1)" >"${fixture_dir}/sha256sum.txt"
make_variant "${absent_digest}"
run_installer
assert_rejected_before_install 'match neither the pin nor'

# --- Case 4: manifest unreachable and pin mismatched -> still fails closed.
manifest_reachable=0
make_variant "${absent_digest}"
run_installer
assert_rejected_before_install 'talosctl digest mismatch'
manifest_reachable=1

# --- Case 5: every call site routes through the script, and none installs raw.
for callsite in \
  .github/actions/deploy-prod/action.yml \
  .github/workflows/ci.yaml \
  .github/workflows/dr-rebuild.yaml \
  .github/workflows/validate-image-verifier-liveness.yaml; do
  if ! grep -q '\.github/scripts/setup-talosctl\.sh' "${repo_root}/${callsite}"; then
    printf 'call site does not use setup-talosctl.sh: %s\n' "${callsite}" >&2
    exit 1
  fi
  if grep -q 'talosctl-linux-amd64' "${repo_root}/${callsite}"; then
    printf 'call site still downloads talosctl directly: %s\n' "${callsite}" >&2
    exit 1
  fi
done

printf 'setup-talosctl verification tests passed\n'
