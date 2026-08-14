#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/test-setup-ksail.XXXXXX")
trap 'rm -rf "${test_root}"' EXIT

fixture_dir="${test_root}/fixtures"
fake_bin="${test_root}/bin"
runner_temp="${test_root}/runner"
mkdir -p "${fixture_dir}" "${fake_bin}" "${runner_temp}"

printf '#!/usr/bin/env bash\nexit 0\n' >"${fixture_dir}/ksail"
chmod +x "${fixture_dir}/ksail"
tar -czf "${fixture_dir}/ksail.tar.gz" -C "${fixture_dir}" ksail
digest=$(sha256sum "${fixture_dir}/ksail.tar.gz" | cut -d' ' -f1)

printf '{"assets":[{"name":"ksail_7.178.25_linux_amd64.tar.gz","digest":"sha256:%s"}]}\n' \
  "${digest}" >"${fixture_dir}/release.json"
printf '%s  ksail_7x178x25_linux_amd64.tar.gz\n' "${digest}" >"${fixture_dir}/checksums.txt"

cat >"${fake_bin}/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail

output=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      output=$2
      shift 2
      ;;
    -H)
      shift 2
      ;;
    -*) shift ;;
    *)
      url=$1
      shift
      ;;
  esac
done

case "${url}" in
  */ksail_7.178.25_linux_amd64.tar.gz)
    cp "${TEST_FIXTURE_DIR}/ksail.tar.gz" "${output}"
    ;;
  */releases/tags/v7.178.25)
    cp "${TEST_FIXTURE_DIR}/release.json" "${output}"
    ;;
  */ksail_7.178.25_checksums.txt)
    cp "${TEST_FIXTURE_DIR}/checksums.txt" "${output}"
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

cat >"${fake_bin}/ksail" <<'FAKE_KSAIL'
#!/usr/bin/env bash
exit 0
FAKE_KSAIL

chmod +x \
  "${fake_bin}/curl" \
  "${fake_bin}/sudo" \
  "${fake_bin}/ksail"

run_installer() {
  if output=$(
    cd "${repo_root}" &&
      PATH="${fake_bin}:${PATH}" \
        RUNNER_TEMP="${runner_temp}" \
        KSAIL_VERSION='7.178.25' \
        TEST_FIXTURE_DIR="${fixture_dir}" \
        TEST_INSTALL_MARKER="${test_root}/installed" \
        .github/scripts/setup-ksail.sh 2>&1
  ); then
    status=0
  else
    status=$?
  fi
}

assert_rejected_before_install() {
  expected_output=$1

  if [ "${status}" -eq 0 ]; then
    printf 'expected setup-ksail.sh to reject invalid release evidence\n' >&2
    exit 1
  fi

  if [ -e "${test_root}/installed" ]; then
    printf 'setup-ksail.sh attempted installation before release verification\n' >&2
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

run_installer
assert_rejected_before_install 'no published checksum for ksail_7.178.25_linux_amd64.tar.gz'

printf '%s  ksail_7.178.25_linux_amd64.tar.gz\n' "${digest}" >"${fixture_dir}/checksums.txt"
printf '{"assets":[{"name":"ksail_7.178.25_linux_amd64.tar.gz","digest":"sha256:%064d"}]}\n' \
  0 >"${fixture_dir}/release.json"

run_installer
assert_rejected_before_install 'release metadata digest mismatch for ksail_7.178.25_linux_amd64.tar.gz'

printf 'setup-ksail dual-source verification tests passed\n'
