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

cat >"${fake_bin}/sha256sum" <<'FAKE_SHA256SUM'
#!/usr/bin/env bash
exit 0
FAKE_SHA256SUM

chmod +x \
  "${fake_bin}/curl" \
  "${fake_bin}/sudo" \
  "${fake_bin}/ksail" \
  "${fake_bin}/sha256sum"

set +e
output=$(
  cd "${repo_root}" &&
    PATH="${fake_bin}:${PATH}" \
      RUNNER_TEMP="${runner_temp}" \
      KSAIL_VERSION='7.178.25' \
      TEST_FIXTURE_DIR="${fixture_dir}" \
      TEST_INSTALL_MARKER="${test_root}/installed" \
      .github/scripts/setup-ksail.sh 2>&1
)
status=$?
set -e

if [ "${status}" -eq 0 ]; then
  printf 'expected setup-ksail.sh to reject a checksum list without the exact asset name\n' >&2
  exit 1
fi

if [ -e "${test_root}/installed" ]; then
  printf 'setup-ksail.sh attempted installation before exact-name verification\n' >&2
  exit 1
fi

case "${output}" in
  *'no published checksum for ksail_7.178.25_linux_amd64.tar.gz'*) ;;
  *)
    printf 'unexpected failure output: %s\n' "${output}" >&2
    exit 1
    ;;
esac

printf 'setup-ksail exact checksum-name test passed\n'
