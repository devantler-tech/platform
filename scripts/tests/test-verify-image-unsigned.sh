#!/usr/bin/env bash
#
# Behaviour tests for scripts/verify-image-unsigned.sh.
#
# The script decides whether the enforcement probe's negative control is still
# unsigned, so its dangerous mistake is answering "unsigned" when it does not
# know. Every case below pins an exit status AND the verdict word, and every
# failure mode — a signature, a referrer, an auth failure, an unexpected status,
# a broken response, a transport error — must come out as anything but 0.
#
# curl is faked. The fake answers like a registry and, like one, refuses any
# read whose credential did not arrive through the curl config file. It also
# records every argument list, so the suite can prove the password and the
# bearer token never appeared in curl's argv, where the process table would
# expose them.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/verify-image-unsigned.sh"

readonly hex='0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
readonly image="ghcr.io/devantler-tech/probe-throwaway-unsigned@sha256:${hex}"

work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT
readonly fake_bin="${work_dir}/bin"
readonly state="${work_dir}/state"
mkdir -p "${fake_bin}"

cases_run=0

fail() {
  printf '\nFAIL: %s\n' "$1" >&2
  exit 1
}

check() {
  cases_run=$((cases_run + 1))
  printf '  ok  %s\n' "$1"
}

cat >"${fake_bin}/curl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
dir="${FAKE_CURL_DIR}"
printf '%s\n' "$*" >>"${dir}/argv.log"

conf='' out='' fmt='' url=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) conf="$2"; shift ;;
    -o) out="$2"; shift ;;
    -w) fmt="$2"; shift ;;
    -H | --max-time) shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done

if [[ -n "${conf}" ]]; then
  printf '%s\n' "${conf}" >>"${dir}/configs.log"
  # Only the ten permission characters: macOS appends `@` or `+` for extended
  # attributes and ACLs, which say nothing about who can read the file.
  ls -l "${conf}" | awk '{print substr($1, 1, 10)}' >>"${dir}/modes.log"
fi
conf_text="$(cat "${conf}" 2>/dev/null || true)"

if [[ -e "${dir}/transport_fail" ]]; then
  [[ "${fmt}" == '%{http_code}' ]] && printf '000'
  exit 7
fi

value() {
  if [[ -e "${dir}/$1" ]]; then cat "${dir}/$1"; else printf '%s' "$2"; fi
}

emit() {
  if [[ -n "${out}" ]]; then printf '%s' "${2:-}" >"${out}"; fi
  [[ "${fmt}" == '%{http_code}' ]] && printf '%s' "$1"
  exit 0
}

case "${url}" in
  */token'?'*)
    [[ "${conf_text}" == *'user = "fake-user:fake-password"'* ]] || emit 401 '{}'
    emit "$(value token_status 200)" "$(value token_body '{"token":"fake-bearer-token"}')"
    ;;
esac

# A registry read carries the bearer token, and only through the config file.
[[ "${conf_text}" == *'Authorization: Bearer fake-bearer-token'* ]] || emit 401 ''

case "${url}" in
  */manifests/sha256:*) emit "$(value digest_status 200)" ;;
  */manifests/sha256-*.sig) emit "$(value sig_status 404)" ;;
  */manifests/sha256-*.att) emit "$(value att_status 404)" ;;
  */manifests/sha256-*.sbom) emit "$(value sbom_status 404)" ;;
  */manifests/sha256-*) emit "$(value tag_status 404)" ;;
  */referrers/*) emit "$(value referrers_status 404)" "$(value referrers_body '')" ;;
esac
emit 599 ''
FAKE
chmod +x "${fake_bin}/curl"

reset_state() {
  rm -rf "${state}"
  mkdir -p "${state}"
}

stage() {
  printf '%s' "$2" >"${state}/$1"
}

out=''
rc=0

# run_verify [--image <ref>] — defaults to the digest ref and the fake credential.
run_verify() {
  local ref="${image}"
  if [[ "${1:-}" == '--image' ]]; then
    ref="$2"
  fi
  set +e
  out="$(FAKE_CURL_DIR="${state}" PATH="${fake_bin}:${PATH}" \
    REGISTRY_USERNAME="${REGISTRY_USERNAME-fake-user}" REGISTRY_PASSWORD="${REGISTRY_PASSWORD-fake-password}" \
    bash "${script}" --image "${ref}" 2>&1)"
  rc=$?
  set -e
}

expect() {
  local what="$1" want_rc="$2" want_text="$3"
  if [[ "${rc}" != "${want_rc}" ]]; then
    fail "${what}: exit ${rc}, expected ${want_rc}. Output: ${out}"
  fi
  grep -qF -- "${want_text}" <<<"${out}" || fail "${what}: output does not contain '${want_text}'. Output: ${out}"
  check "${what}"
}

printf 'test-verify-image-unsigned\n'

[[ -f "${script}" ]] || fail "missing ${script}"

# --- Unsigned: the only exit 0 --------------------------------------------------
reset_state
run_verify
expect 'an image with no signature tags and no referrers API is UNSIGNED' 0 'UNSIGNED:'

reset_state
stage referrers_status 200
stage referrers_body '{"schemaVersion":2,"manifests":[]}'
run_verify
expect 'an empty referrers index is UNSIGNED' 0 'UNSIGNED:'

# --- Secrets never reach curl's argv ---------------------------------------------
reset_state
run_verify
[[ "${rc}" == 0 ]] || fail "argv case should succeed first: ${out}"
[[ -s "${state}/argv.log" ]] || fail 'the fake curl was never called'
if grep -qF 'fake-password' "${state}/argv.log"; then
  fail 'the registry password appeared in a curl argument list'
fi
if grep -qF 'fake-bearer-token' "${state}/argv.log"; then
  fail 'the bearer token appeared in a curl argument list'
fi
if grep -qvxF -- '-rw-------' "${state}/modes.log"; then
  fail "a curl config file was readable by others: $(sort -u "${state}/modes.log" | tr '\n' ' ')"
fi
while IFS= read -r conf_path; do
  [[ ! -e "${conf_path}" ]] || fail "the curl config file ${conf_path} was left behind"
done <"${state}/configs.log"
check 'the password and token travel only in a 0600 config file that is removed afterwards'

# --- Signed: exit 1 ----------------------------------------------------------------
for tag_case in tag_status sig_status att_status sbom_status; do
  reset_state
  stage "${tag_case}" 200
  run_verify
  expect "a digest tag present (${tag_case%_status}) is SIGNED" 1 'SIGNED:'
done

reset_state
stage referrers_status 200
stage referrers_body '{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aa","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json"}]}'
run_verify
expect 'a referrer attached to the digest is SIGNED' 1 'OCI referrer'

# --- Unknown: exit 3, never unsigned ------------------------------------------------
reset_state
stage token_status 401
run_verify
expect 'a refused token exchange is UNKNOWN' 3 'refused the credential'

reset_state
REGISTRY_PASSWORD='wrong-password' run_verify
expect 'a wrong credential is UNKNOWN' 3 'refused the credential'

reset_state
stage token_body '{}'
run_verify
expect 'a token response without a token is UNKNOWN' 3 'returned no token'

reset_state
stage digest_status 403
run_verify
expect 'a digest the credential cannot read is UNKNOWN' 3 'is not readable'

reset_state
stage digest_status 404
run_verify
expect 'a digest that does not exist is UNKNOWN' 3 'is not readable'

reset_state
stage sig_status 500
run_verify
expect 'a signature tag lookup answering 500 is UNKNOWN' 3 'could not establish whether'

reset_state
stage att_status 401
run_verify
expect 'a signature tag lookup answering 401 is UNKNOWN' 3 'could not establish whether'

reset_state
stage referrers_status 502
run_verify
expect 'a referrers lookup answering 502 is UNKNOWN' 3 'could not read referrers'

reset_state
stage referrers_status 200
stage referrers_body 'not json'
run_verify
expect 'a malformed referrers response is UNKNOWN' 3 'not a valid index'

reset_state
stage referrers_status 200
stage referrers_body '{"schemaVersion":2}'
run_verify
expect 'a referrers response without a manifests list is UNKNOWN' 3 'not a valid index'

reset_state
: >"${state}/transport_fail"
run_verify
expect 'a transport failure is UNKNOWN' 3 'UNKNOWN:'

# --- Usage: exit 2, and no request is made ---------------------------------------------
for bad_ref in \
  'ghcr.io/devantler-tech/probe-throwaway-unsigned:run-1-1' \
  "ghcr.io/devantler-tech/probe-throwaway-unsigned@sha256:${hex:0:63}" \
  "ghcr.io/devantler-tech/probe-throwaway-unsigned@sha256:${hex}0" \
  "ghcr.io/devantler-tech/probe-throwaway-unsigned@sha256:$(printf '%s' "${hex}" | tr 'a-f' 'A-F')" \
  "ghcr.io/Devantler-Tech/probe-throwaway-unsigned@sha256:${hex}" \
  "sha256:${hex}" \
  "ghcr.io/devantler-tech/probe-throwaway-unsigned:run-1-1@sha256:${hex}"; do
  reset_state
  run_verify --image "${bad_ref}"
  [[ "${rc}" == 2 ]] || fail "malformed ref '${bad_ref}' should exit 2, got ${rc}: ${out}"
  grep -qF 'is not <registry>/<repository>@sha256:' <<<"${out}" || fail "malformed ref '${bad_ref}': wrong message: ${out}"
  [[ ! -e "${state}/argv.log" ]] || fail "malformed ref '${bad_ref}' still called the registry"
done
check 'a tag, a short, long or upper-case digest, or a ref without a repository is a usage error, before any request'

reset_state
REGISTRY_PASSWORD='' run_verify
expect 'a missing credential is a usage error' 2 'must both be set'
[[ ! -e "${state}/argv.log" ]] || fail 'a missing credential still called the registry'

# --- Wiring ---------------------------------------------------------------------------
ci="${root_dir}/.github/workflows/ci.yaml"
grep -qF 'bash scripts/tests/test-verify-image-unsigned.sh' "${ci}" || fail 'ci.yaml does not run this test'
check 'ci.yaml runs this test'

printf '\nAll %d case(s) passed: verify-image-unsigned.sh behaviour is pinned.\n' "${cases_run}"
