#!/bin/sh
# List one side of the Wedding backup catalogue and hash the objects the runner
# names, so the dedicated bucket can be shown to cover the stale shared copy
# (#4481).
#
# Runs in the two pods that scripts/verify-wedding-shared-backup-coverage.sh
# creates: one beside the shared backup credential, one beside the dedicated
# one. No namespace holds both since #3253. It is delivered as a ConfigMap and
# executed by the digest-pinned mc image with a static BusyBox toolbox from an
# init container. It is POSIX sh and uses only the tools checked below.
#
# COMMANDS. The container runs `serve`, which checks the configuration, stores
# the credential and waits. The runner then drives the rest with `kubectl exec`:
#   list   write the catalogue listing to WORK_DIR/listing
#   hash   read keys on stdin and write their sha256 to WORK_DIR/sums
#
# READ ONLY. The only mc commands it runs are `alias set`, `ls` and `cat`. It
# never writes to or deletes from either bucket, and never prints a credential
# or the account-specific endpoint host.

set -eu

fail() {
  printf 'coverage-pod: %s\n' "$1" >&2
  exit 1
}

readonly reviewed_prefix='cnpg/wedding-db'
readonly dedicated_bucket='wedding-db-backups'

for tool in mc sed grep tail sha256sum cut date cat sleep mkdir rm; do
  command -v "${tool}" >/dev/null 2>&1 || fail "required tool missing from the image: ${tool}"
done

: "${ENDPOINT:?ENDPOINT is required}"
: "${ROLE:?ROLE is required}"
: "${BUCKET:?BUCKET is required}"
: "${PREFIX:?PREFIX is required}"

case "${ENDPOINT}" in
  https://*) ;;
  *) fail "the R2 endpoint must be https" ;;
esac
[ "${PREFIX}" = "${reviewed_prefix}" ] || fail "the catalogue prefix is not the reviewed one"
case "${ROLE}" in
  shared)
    [ "${BUCKET}" != "${dedicated_bucket}" ] || fail "the shared side names the dedicated bucket"
    ;;
  dedicated)
    [ "${BUCKET}" = "${dedicated_bucket}" ] || fail "the dedicated side names another bucket"
    ;;
  *) fail "the role must be shared or dedicated" ;;
esac

credentials="${CREDENTIALS_DIR:-/credentials}"
work="${WORK_DIR:-/tmp/coverage}"
mkdir -p "${work}"
export MC_CONFIG_DIR="${MC_CONFIG_DIR:-${work}/.mc}"
mc_err="${work}/mc.err"

# redact_errors prints the last lines of an mc error log without the endpoint
# host (with or without its scheme), any IPv4 address, or any long hex token such
# as an access key ID or request ID. These lines reach a public workflow log.
host="${ENDPOINT#https://}"
redact_errors() {
  sed -e 's#https\{0,1\}://[^/ "`]*#<endpoint>#g' -e "s#${host}#<endpoint>#g" \
    -e 's/[0-9a-fA-F]\{32,\}/<hex>/g' \
    -e 's/[0-9]\{1,3\}\(\.[0-9]\{1,3\}\)\{3\}\(:[0-9]\{1,5\}\)\{0,1\}/<ip>/g' "${mc_err}" | tail -n 5 >&2
}

command="${1:-}"
case "${command}" in
  serve)
    [ "$#" -eq 1 ] || fail "serve takes no arguments"
    access_key="$(cat "${credentials}/ACCESS_KEY_ID")"
    [ -n "${access_key}" ] || fail "the credential is empty"
    if ! mc alias set store "${ENDPOINT}" "${access_key}" \
      "$(cat "${credentials}/SECRET_ACCESS_KEY")" --api S3v4 >/dev/null 2>"${mc_err}"; then
      redact_errors
      fail "could not configure the ${ROLE} credential"
    fi
    unset access_key
    printf '==== COVERAGE POD READY ====\n'
    waited=0
    while [ "${waited}" -lt "${SERVE_TIMEOUT:-3000}" ]; do
      sleep 5
      waited=$((waited + 5))
    done
    ;;

  list)
    [ "$#" -eq 1 ] || fail "list takes no arguments"
    output="${work}/listing"
    # Truncating to the second can only move the start earlier, which is the
    # side the evaluator's ordering check is strict about.
    started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if ! mc ls --json --recursive "store/${BUCKET}/${PREFIX}/" >"${output}.raw" 2>"${mc_err}" </dev/null; then
      redact_errors
      fail "listing the ${ROLE} catalogue failed"
    fi

    : >"${output}.partial"
    files=0
    while IFS= read -r line; do
      [ -n "${line}" ] || continue
      case "${line}" in
        *'"status":"success"'*) ;;
        *) fail "listing the ${ROLE} catalogue returned a record that is not a success" ;;
      esac
      case "${line}" in
        *'"type":"folder"'*) continue ;;
        *'"type":"file"'*) ;;
        *) fail "listing the ${ROLE} catalogue returned an unexpected record type" ;;
      esac

      line="$(printf '%s' "${line}" | sed -e 's/"url":"[^"]*",//' -e 's/,"url":"[^"]*"//')"
      key="$(printf '%s' "${line}" | sed -n 's/.*"key":"\([^"\\]*\)".*/\1/p')"
      [ -n "${key}" ] || fail "listing the ${ROLE} catalogue returned an object without a plain key"

      printf '%s\n' "${line}" >>"${output}.partial"
      files=$((files + 1))
    done <"${output}.raw"

    printf '{"status":"success","type":"listing-complete","location":"%s/%s","started":"%s","files":%d}\n' \
      "${BUCKET}" "${PREFIX}" "${started}" "${files}" >>"${output}.partial"
    cat "${output}.partial" >"${output}"
    printf 'coverage-pod: listed %s objects on the %s side\n' "${files}" "${ROLE}"
    ;;

  hash)
    [ "$#" -eq 1 ] || fail "hash takes no arguments"
    [ -e "${work}/listing" ] || fail "hash needs a listing"
    : >"${work}/sums.partial"
    hashed=0
    while IFS= read -r key; do
      [ -n "${key}" ] || continue
      grep -qF "\"key\":\"${key}\"" "${work}/listing" || fail "asked to hash a key the listing does not hold"
      rm -f "${work}/cat-failed"
      sum="$({ mc cat "store/${BUCKET}/${PREFIX}/${key}" </dev/null 2>"${mc_err}" ||
        : >"${work}/cat-failed"; } | sha256sum | cut -d ' ' -f 1)"
      if [ -e "${work}/cat-failed" ]; then
        redact_errors
        fail "could not read ${key} to checksum it"
      fi
      printf '%s' "${sum}" | grep -Eq '^[0-9a-f]{64}$' || fail "malformed checksum for ${key}"
      printf '%s\t%s\n' "${key}" "${sum}" >>"${work}/sums.partial"
      hashed=$((hashed + 1))
    done
    cat "${work}/sums.partial" >"${work}/sums"
    printf 'coverage-pod: hashed %s objects on the %s side\n' "${hashed}" "${ROLE}"
    ;;

  *) fail "unknown command" ;;
esac
