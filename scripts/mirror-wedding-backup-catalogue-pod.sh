#!/bin/sh
# Copy the shared Wedding backup catalogue into its dedicated bucket, from inside
# the wedding-app namespace, and emit the three listings the evaluator judges.
#
# Runs in the pod that scripts/mirror-wedding-backup-catalogue.sh creates. It is
# delivered as a ConfigMap and executed by the digest-pinned mc image, so it is
# POSIX sh and uses only the tools checked below.
#
# WHY IN THE CLUSTER. Both credentials already live in wedding-app as Secrets,
# and the tenant's CiliumNetworkPolicy already allows every pod there to reach
# *.r2.cloudflarestorage.com. Running here keeps the credentials inside the
# cluster and needs no network policy change.
#
# WHAT IT NEVER DOES. It never writes to the source, never deletes anything in
# the destination (no `mc mirror --remove`), and never prints a credential or
# the account-specific endpoint host.
#
# Output on stdout, in this order, and nothing else:
#   ==== RUN-START <RFC 3339> ====
#   ==== BEGIN source-before ==== ... ==== END source-before ====
#   ==== BEGIN source-after ==== ... ==== END source-after ====
#   ==== BEGIN destination ==== ... ==== END destination ====
# Each listing is `mc ls --json --recursive` output with the url field removed, a
# sha256 field added to every multipart object (whose ETag is not a content
# checksum), and one completion record appended only after the listing succeeded.

set -eu

fail() {
  printf 'mirror-pod: %s\n' "$1" >&2
  exit 1
}

for tool in mc sed grep sha256sum cut date cat; do
  command -v "${tool}" >/dev/null 2>&1 || fail "required tool missing from the image: ${tool}"
done

: "${ENDPOINT:?ENDPOINT is required}"
: "${SOURCE_BUCKET:?SOURCE_BUCKET is required}"
: "${SOURCE_PREFIX:?SOURCE_PREFIX is required}"
: "${DESTINATION_BUCKET:?DESTINATION_BUCKET is required}"
: "${DESTINATION_PREFIX:?DESTINATION_PREFIX is required}"

case "${ENDPOINT}" in
  https://*) ;;
  *) fail "the R2 endpoint must be https" ;;
esac

credentials="${CREDENTIALS_DIR:-/credentials}"
work="${WORK_DIR:-/tmp/mirror}"
mkdir -p "${work}"
export MC_CONFIG_DIR="${MC_CONFIG_DIR:-${work}/.mc}"

source_id="$(cat "${credentials}/source/ACCESS_KEY_ID")"
destination_id="$(cat "${credentials}/destination/ACCESS_KEY_ID")"
[ -n "${source_id}" ] && [ -n "${destination_id}" ] || fail "a credential is empty"
# Each credential is scoped to its own bucket. One key on both sides means the
# plan is not the reviewed two-identity copy, whatever the Secret names say.
[ "${source_id}" != "${destination_id}" ] ||
  fail "credential reuse: source and destination use the same access key"

mc alias set source "${ENDPOINT}" "${source_id}" \
  "$(cat "${credentials}/source/SECRET_ACCESS_KEY")" --api S3v4 >/dev/null ||
  fail "could not configure the source credential"
mc alias set destination "${ENDPOINT}" "${destination_id}" \
  "$(cat "${credentials}/destination/SECRET_ACCESS_KEY")" --api S3v4 >/dev/null ||
  fail "could not configure the destination credential"
unset source_id destination_id

utc_now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

# list <alias> <bucket> <prefix> <output>
list() {
  alias_name="$1"
  bucket="$2"
  prefix="$3"
  output="$4"

  # Truncating to the second can only move the start EARLIER, which classifies
  # an object written in that second as archived during the run — the side the
  # evaluator verifies rather than the side it trusts.
  started="$(utc_now)"
  mc ls --json --recursive "${alias_name}/${bucket}/${prefix}/" >"${output}.raw" </dev/null ||
    fail "listing ${alias_name} failed"

  : >"${output}"
  files=0
  while IFS= read -r line; do
    [ -n "${line}" ] || continue
    case "${line}" in
      *'"status":"success"'*) ;;
      *) fail "listing ${alias_name} returned a record that is not a success" ;;
    esac
    case "${line}" in
      *'"type":"folder"'*) continue ;;
      *'"type":"file"'*) ;;
      *) fail "listing ${alias_name} returned an unexpected record type" ;;
    esac

    line="$(printf '%s' "${line}" | sed -e 's/"url":"[^"]*",//' -e 's/,"url":"[^"]*"//')"
    key="$(printf '%s' "${line}" | sed -n 's/.*"key":"\([^"\\]*\)".*/\1/p')"
    etag="$(printf '%s' "${line}" | sed -n 's/.*"etag":"\([^"\\]*\)".*/\1/p')"
    [ -n "${key}" ] || fail "listing ${alias_name} returned an object without a plain key"

    case "${etag}" in
      *-*)
        rm -f "${work}/cat-failed"
        sha="$({ mc cat "${alias_name}/${bucket}/${prefix}/${key}" </dev/null ||
          : >"${work}/cat-failed"; } | sha256sum | cut -d ' ' -f 1)"
        [ ! -e "${work}/cat-failed" ] || fail "could not read ${key} from ${alias_name} to checksum it"
        printf '%s' "${sha}" | grep -Eq '^[0-9a-f]{64}$' || fail "malformed checksum for ${key}"
        line="$(printf '%s' "${line}" | sed "s/^{/{\"sha256\":\"${sha}\",/")"
        ;;
    esac

    printf '%s\n' "${line}" >>"${output}"
    files=$((files + 1))
  done <"${output}.raw"

  printf '{"status":"success","type":"listing-complete","location":"%s/%s","started":"%s","files":%d}\n' \
    "${bucket}" "${prefix}" "${started}" "${files}" >>"${output}"
}

list source "${SOURCE_BUCKET}" "${SOURCE_PREFIX}" "${work}/source-before"
run_start="$(sed -n '$s/.*"started":"\([^"]*\)".*/\1/p' "${work}/source-before")"
[ -n "${run_start}" ] || fail "the starting listing has no start time"

# --overwrite replaces a destination object that differs from the source, so a
# re-run repairs a damaged copy. There is deliberately no --remove.
if ! mc mirror --overwrite "source/${SOURCE_BUCKET}/${SOURCE_PREFIX}/" \
  "destination/${DESTINATION_BUCKET}/${DESTINATION_PREFIX}/" >/dev/null 2>"${work}/mirror.err" </dev/null; then
  sed -e 's#https://[^/ ]*#<endpoint>#g' "${work}/mirror.err" | tail -n 5 >&2
  fail "the copy did not complete"
fi

# The source is listed again BEFORE the destination, because the evaluator
# refuses a destination listing that started earlier than the ending listing.
list source "${SOURCE_BUCKET}" "${SOURCE_PREFIX}" "${work}/source-after"
list destination "${DESTINATION_BUCKET}" "${DESTINATION_PREFIX}" "${work}/destination"

emit() {
  printf '==== BEGIN %s ====\n' "$1"
  cat "$2"
  printf '==== END %s ====\n' "$1"
}

printf '==== RUN-START %s ====\n' "${run_start}"
emit source-before "${work}/source-before"
emit source-after "${work}/source-after"
emit destination "${work}/destination"
