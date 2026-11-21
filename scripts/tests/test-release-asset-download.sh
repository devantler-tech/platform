#!/usr/bin/env bash
# Offline tests for .github/scripts/release-asset-download.sh (#5942).
#
# curl is replaced by a fake that serves fixtures by URL and records every
# request, so each case asserts which path served the asset, what the log line
# says, and that the token never reaches the log or the anonymous request.
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
lib="${repo_root}/.github/scripts/release-asset-download.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/test-release-asset-download.XXXXXX")
trap 'rm -rf "${test_root}"' EXIT
mkdir -p "${test_root}/bin" "${test_root}/fixtures" "${test_root}/runner"
export TEST_ROOT="${test_root}"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

printf 'release bytes\n' >"${test_root}/fixtures/asset"
printf '{"assets":[{"name":"other.tar.gz","id":11},{"name":"tool-linux-amd64","id":42}]}\n' \
  >"${test_root}/fixtures/release.json"
printf '{"assets":[{"name":"other.tar.gz","id":11}]}\n' >"${test_root}/fixtures/release-unlisted.json"

# The fake answers per URL. TEST_FAIL names the requests that fail with a 504:
# a space-separated list of `metadata`, `asset-api` and `anonymous`.
cat >"${test_root}/bin/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
url='' output='' write_out='' retry=0 auth=no
while (($#)); do
  case "$1" in
    --output | -o) output=$2; shift 2 ;;
    --write-out | -w) write_out=$2; shift 2 ;;
    --retry) retry=$2; shift 2 ;;
    --retry-delay | --retry-max-time | --proto | --proto-redir) shift 2 ;;
    -H)
      [[ $2 != Authorization:* ]] || auth=yes
      shift 2
      ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "${url}" in
  https://api.github.com/repos/example/tool/releases/tags/v1.2.3) kind=metadata ;;
  https://api.github.com/repos/example/tool/releases/assets/42) kind=asset-api ;;
  https://github.com/example/tool/releases/download/v1.2.3/tool-linux-amd64) kind=anonymous ;;
  *) printf 'unexpected URL: %s\n' "${url}" >&2; exit 2 ;;
esac
printf '%s auth=%s retry=%s\n' "${kind}" "${auth}" "${retry}" >>"${TEST_ROOT}/requests"
code() { [[ ${write_out} != '%{http_code}' ]] || printf '%s' "$1"; }
if [[ " ${TEST_FAIL:-} " == *" ${kind} "* ]]; then
  # Like curl --fail: a partial body may be left behind, and the code is still written.
  printf 'partial' >"${output}"
  code 504
  printf 'curl: (22) The requested URL returned error: 504\n' >&2
  exit 22
fi
case "${kind}" in
  metadata) cp "${TEST_ROOT}/fixtures/${TEST_RELEASE_JSON:-release.json}" "${output}" ;;
  *) cp "${TEST_ROOT}/fixtures/asset" "${output}" ;;
esac
code 200
FAKE_CURL
chmod +x "${test_root}/bin/curl"

token='test-token-value-never-logged'
out="${test_root}/runner/asset"

# run TOKEN — downloads the fixture asset with the given GITHUB_TOKEN ('' = none).
run() {
  rm -f "${test_root}/requests" "${out}"
  status=0
  log=$(
    PATH="${test_root}/bin:${PATH}" RUNNER_TEMP="${test_root}/runner" GITHUB_TOKEN="$1" \
      bash -c 'set -euo pipefail; source "$1"; release_asset_download example/tool v1.2.3 tool-linux-amd64 "$2"' \
      _ "${lib}" "${out}" 2>&1
  ) || status=$?
  [[ ${log} != *"${token}"* ]] || fail "the token reached the log: ${log}"
  # The anonymous URL must never carry the token.
  if [[ -e ${test_root}/requests ]] && grep -q '^anonymous auth=yes' "${test_root}/requests"; then
    fail 'the anonymous request carried the Authorization header'
  fi
  # Every request retries transient failures (#4549).
  if [[ -e ${test_root}/requests ]] && grep -qv 'retry=[1-9]' "${test_root}/requests"; then
    fail "a request does not retry transient failures: $(cat "${test_root}/requests")"
  fi
  [[ -z $(find "${test_root}/runner" -name 'release-asset.*' -print -quit) ]] ||
    fail 'the metadata scratch file was left behind'
}

requests() { tr '\n' ' ' <"${test_root}/requests" | sed 's/ $//'; }

assert_served() { # path
  [[ ${status} == 0 ]] || fail "expected success, got ${status}: ${log}"
  cmp -s "${test_root}/fixtures/asset" "${out}" || fail 'the served bytes are not the asset'
  [[ ${log} == *"path=$1 status=200 result=served"* ]] || fail "expected the $1 path to serve it: ${log}"
}

# --- 1. Authenticated success: the asset API serves it, the public URL is never asked.
run "${token}"
assert_served authenticated
[[ $(requests) == 'metadata auth=yes retry=5 asset-api auth=yes retry=5' ]] ||
  fail "unexpected requests for authenticated success: $(requests)"
[[ $(grep -c '^release-asset ' <<<"${log}") == 1 ]] || fail "expected exactly one log line: ${log}"

# --- 2. Authenticated download fails, anonymous succeeds.
TEST_FAIL='asset-api' run "${token}"
assert_served anonymous
[[ ${log} == *'release-asset asset=tool-linux-amd64 path=authenticated status=504 result=failed'* ]] ||
  fail "the authenticated failure was not logged with its status: ${log}"
[[ $(requests) == 'metadata auth=yes retry=5 asset-api auth=yes retry=5 anonymous auth=no retry=5' ]] ||
  fail "unexpected requests for the fallback: $(requests)"

# --- 3. Authenticated metadata read fails, anonymous succeeds without asking for the asset.
TEST_FAIL='metadata' run "${token}"
assert_served anonymous
[[ ${log} == *'path=authenticated status=504 result=failed'* ]] || fail "metadata failure not logged: ${log}"
[[ $(requests) == 'metadata auth=yes retry=5 anonymous auth=no retry=5' ]] ||
  fail "unexpected requests after a metadata failure: $(requests)"

# --- 4. The release does not list the asset: logged as unlisted, then anonymous.
TEST_RELEASE_JSON='release-unlisted.json' run "${token}"
assert_served anonymous
[[ ${log} == *'path=authenticated status=200 result=unlisted'* ]] || fail "unlisted asset not logged: ${log}"

# --- 5. Both paths fail: the call fails, names both, and leaves no partial file.
TEST_FAIL='asset-api anonymous' run "${token}"
[[ ${status} != 0 ]] || fail 'a download neither path served was reported as success'
[[ ! -e ${out} ]] || fail 'a failed download left a partial file behind'
[[ ${log} == *'path=authenticated status=504 result=failed'* ]] || fail "authenticated failure not logged: ${log}"
[[ ${log} == *'path=anonymous status=504 result=failed'* ]] || fail "anonymous failure not logged: ${log}"

# --- 6. No token: straight to the public URL, nothing sent to the API.
run ''
assert_served anonymous
[[ $(requests) == 'anonymous auth=no retry=5' ]] || fail "a tokenless download asked the API: $(requests)"

# --- 7. No token and the public URL fails: the call fails.
TEST_FAIL='anonymous' run ''
[[ ${status} != 0 ]] || fail 'a failed tokenless download was reported as success'
[[ ! -e ${out} ]] || fail 'a failed tokenless download left a partial file behind'

# --- 8. Every helper that pulls a GitHub release asset sources and calls this
# function. Each helper's own test asserts which URLs it requests.
expected_source="source \"\$(dirname \"\${BASH_SOURCE[0]}\")/release-asset-download.sh\""
for helper in setup-talosctl.sh setup-supply-chain-tools.sh setup-ksail.sh; do
  path="${repo_root}/.github/scripts/${helper}"
  grep -Fqx "${expected_source}" "${path}" ||
    fail "${helper} does not source the shared download function"
  grep -q '^release_asset_download ' "${path}" || fail "${helper} does not call the shared download function"
done

printf 'release-asset download tests passed\n'
