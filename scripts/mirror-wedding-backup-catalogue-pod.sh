#!/bin/sh
# Copy the shared Wedding backup catalogue into its dedicated bucket, from inside
# the wedding-app namespace, and write the three listings the evaluator judges.
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
# OUTPUT. Files in WORK_DIR, collected by the runner with `kubectl exec`:
#   run-start       the RFC 3339 start of the starting listing
#   source-before   \
#   source-after     > `mc ls --json --recursive` output with the url field removed
#   destination     /  and one completion record appended after a successful listing
# Container logs are not used for the listings because the kubelet rotates them,
# which could silently drop the first listing of a large catalogue. Once the files
# are complete the script prints `==== LISTINGS READY ====` and stays alive for
# COLLECT_TIMEOUT seconds so the runner can read them.
#
# CHECKSUMS. A multipart ETag is not a content checksum, and the copy is free to
# upload an object differently from the original, so an object can be multipart on
# one side and single-part on the other. Every key that is multipart on ANY side
# therefore gets a sha256 of its bytes on EVERY side it exists on.

set -eu

fail() {
  printf 'mirror-pod: %s\n' "$1" >&2
  exit 1
}

for tool in mc sed grep awk sort tail sha256sum cut date cat sleep; do
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
mc_err="${work}/mc.err"

# redact_errors prints the last lines of an mc error log without the endpoint host.
redact_errors() {
  sed -e 's#https://[^/ ]*#<endpoint>#g' "${mc_err}" | tail -n 5 >&2
}

source_id="$(cat "${credentials}/source/ACCESS_KEY_ID")"
destination_id="$(cat "${credentials}/destination/ACCESS_KEY_ID")"
if [ -z "${source_id}" ] || [ -z "${destination_id}" ]; then
  fail "a credential is empty"
fi
# Each credential is scoped to its own bucket. One key on both sides means the
# plan is not the reviewed two-identity copy, whatever the Secret names say.
[ "${source_id}" != "${destination_id}" ] ||
  fail "credential reuse: source and destination use the same access key"

if ! mc alias set source "${ENDPOINT}" "${source_id}" \
  "$(cat "${credentials}/source/SECRET_ACCESS_KEY")" --api S3v4 >/dev/null 2>"${mc_err}"; then
  redact_errors
  fail "could not configure the source credential"
fi
if ! mc alias set destination "${ENDPOINT}" "${destination_id}" \
  "$(cat "${credentials}/destination/SECRET_ACCESS_KEY")" --api S3v4 >/dev/null 2>"${mc_err}"; then
  redact_errors
  fail "could not configure the destination credential"
fi
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
  if ! mc ls --json --recursive "${alias_name}/${bucket}/${prefix}/" >"${output}.raw" 2>"${mc_err}" </dev/null; then
    redact_errors
    fail "listing ${alias_name} failed"
  fi

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
    [ -n "${key}" ] || fail "listing ${alias_name} returned an object without a plain key"

    printf '%s\n' "${line}" >>"${output}"
    files=$((files + 1))
  done <"${output}.raw"

  printf '{"status":"success","type":"listing-complete","location":"%s/%s","started":"%s","files":%d}\n' \
    "${bucket}" "${prefix}" "${started}" "${files}" >>"${output}"
}

# checksum <alias> <bucket> <prefix> <key> prints the sha256 of the object bytes.
checksum() {
  rm -f "${work}/cat-failed"
  sum="$({ mc cat "$1/$2/$3/$4" </dev/null 2>"${mc_err}" ||
    : >"${work}/cat-failed"; } | sha256sum | cut -d ' ' -f 1)"
  if [ -e "${work}/cat-failed" ]; then
    redact_errors
    fail "could not read $4 from $1 to checksum it"
  fi
  printf '%s' "${sum}" | grep -Eq '^[0-9a-f]{64}$' || fail "malformed checksum for $4"
  printf '%s' "${sum}"
}

# hash_side <alias> <bucket> <prefix> <map> <listing>... writes "<key>\t<sha256>"
# for every multipart key that appears in any of the listings.
hash_side() {
  alias_name="$1"
  bucket="$2"
  prefix="$3"
  map="$4"
  shift 4
  : >"${map}"
  while IFS= read -r key; do
    if cat "$@" | grep -qF "\"key\":\"${key}\""; then
      sum="$(checksum "${alias_name}" "${bucket}" "${prefix}" "${key}")" || exit 1
      printf '%s\t%s\n' "${key}" "${sum}" >>"${map}"
    fi
  done <"${work}/multipart-keys"
}

# add_checksums <map> <listing> injects the sha256 field into matching object records.
# The map is recognised by file name, not by `NR == FNR`: an empty map (a side
# with no multipart objects) would otherwise swallow the whole listing as map
# entries and leave it empty.
add_checksums() {
  awk -F '\t' '
    FILENAME == ARGV[1] { sum[$1] = $2; next }
    /"type":"file"/ && match($0, /"key":"[^"]*"/) {
      key = substr($0, RSTART + 7, RLENGTH - 8)
      if (key in sum) sub(/^\{/, "{\"sha256\":\"" sum[key] "\",")
    }
    { print }
  ' "$1" "$2" >"$2.sums" || fail "could not add checksums to $2"
  cat "$2.sums" >"$2"
}

list source "${SOURCE_BUCKET}" "${SOURCE_PREFIX}" "${work}/source-before"
run_start="$(sed -n '$s/.*"started":"\([^"]*\)".*/\1/p' "${work}/source-before")"
[ -n "${run_start}" ] || fail "the starting listing has no start time"

# --overwrite copies a destination object again when its size or modification
# time differs from the source. It cannot see a same-size corruption: that is
# reported by the evaluator as a checksum mismatch, and the damaged destination
# object has to be removed by hand before the next run. There is deliberately no
# --remove.
if ! mc mirror --overwrite "source/${SOURCE_BUCKET}/${SOURCE_PREFIX}/" \
  "destination/${DESTINATION_BUCKET}/${DESTINATION_PREFIX}/" >/dev/null 2>"${mc_err}" </dev/null; then
  redact_errors
  fail "the copy did not complete"
fi

# The source is listed again BEFORE the destination, because the evaluator
# refuses a destination listing that started earlier than the ending listing.
list source "${SOURCE_BUCKET}" "${SOURCE_PREFIX}" "${work}/source-after"
list destination "${DESTINATION_BUCKET}" "${DESTINATION_PREFIX}" "${work}/destination"

grep -h '"type":"file"' "${work}/source-before" "${work}/source-after" "${work}/destination" |
  grep '"etag":"[^"]*-[^"]*"' |
  sed -n 's/.*"key":"\([^"\\]*\)".*/\1/p' |
  sort -u >"${work}/multipart-keys"

hash_side source "${SOURCE_BUCKET}" "${SOURCE_PREFIX}" "${work}/sums-source" \
  "${work}/source-before" "${work}/source-after"
hash_side destination "${DESTINATION_BUCKET}" "${DESTINATION_PREFIX}" "${work}/sums-destination" \
  "${work}/destination"
add_checksums "${work}/sums-source" "${work}/source-before"
add_checksums "${work}/sums-source" "${work}/source-after"
add_checksums "${work}/sums-destination" "${work}/destination"

printf '%s\n' "${run_start}" >"${work}/run-start"
printf '==== LISTINGS READY ====\n'

waited=0
while [ "${waited}" -lt "${COLLECT_TIMEOUT:-1800}" ]; do
  sleep 5
  waited=$((waited + 5))
done
