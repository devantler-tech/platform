#!/bin/sh
# Prove, from inside the wedding-app namespace, that the dedicated Wedding backup
# credential is refused by the shared backup destination.
#
# Runs in the pod that scripts/verify-wedding-backup-denial.sh creates. It is
# delivered as a ConfigMap and executed by the digest-pinned mc image with a
# static BusyBox toolbox from an init container, like the catalogue mirror. It is
# POSIX sh and uses only the tools checked below.
#
# WHAT IT PROVES, refusing at the first precondition it cannot prove:
#   1. The dedicated credential lists its own catalogue. A refusal observed later
#      therefore cannot come from a broken key, endpoint or network path.
#   2. The shared credential lists the shared catalogue and names one object in
#      it. The refused targets therefore exist, so a mistyped bucket or an empty
#      prefix cannot pass as a denial.
#   3. With the dedicated credential, listing the shared catalogue, reading that
#      object and writing a probe object to the shared bucket are each refused
#      with the S3 error code AccessDenied. All three are attempted and reported.
#      A success means the isolation is broken. Any other error is not evidence
#      of denial, so it fails the proof as unproven.
#
# WHAT IT NEVER DOES. It never prints a credential or the account-specific
# endpoint host, and it writes nothing except the probe object the dedicated
# credential must be refused. If that write is accepted anyway, the shared
# credential removes the probe object before the proof fails.
#
# OUTPUT. On success, one receipt line followed by `==== DENIAL OBSERVED ====`.
# Every refusal is a `denial-pod: ` line on stderr and a non-zero exit.

set -eu

fail() {
  printf 'denial-pod: %s\n' "$1" >&2
  exit 1
}

for tool in mc sed grep tr sort head tail cat rm mkdir; do
  command -v "${tool}" >/dev/null 2>&1 || fail "required tool missing from the image: ${tool}"
done

: "${ENDPOINT:?ENDPOINT is required}"
: "${SHARED_BUCKET:?SHARED_BUCKET is required}"
: "${SHARED_PREFIX:?SHARED_PREFIX is required}"
: "${DEDICATED_BUCKET:?DEDICATED_BUCKET is required}"
: "${DEDICATED_PREFIX:?DEDICATED_PREFIX is required}"
: "${PROBE_ID:?PROBE_ID is required}"

case "${ENDPOINT}" in
  https://*) ;;
  *) fail "the R2 endpoint must be https" ;;
esac
[ "${SHARED_BUCKET}" != "${DEDICATED_BUCKET}" ] ||
  fail "the shared and dedicated destinations are the same bucket"
printf '%s' "${PROBE_ID}" | grep -Eq '^[a-z0-9-]{1,40}$' || fail "the probe id is not a valid name fragment"

credentials="${CREDENTIALS_DIR:-/credentials}"
work="${WORK_DIR:-/tmp/denial}"
mkdir -p "${work}"
export MC_CONFIG_DIR="${MC_CONFIG_DIR:-${work}/.mc}"
mc_err="${work}/mc.err"

# redact prints the last lines of an mc log without the endpoint host.
redact() {
  sed -e 's#https://[^/ "]*#<endpoint>#g' "$1" | tail -n 5 >&2
}

shared_id="$(cat "${credentials}/shared/ACCESS_KEY_ID")"
dedicated_id="$(cat "${credentials}/dedicated/ACCESS_KEY_ID")"
if [ -z "${shared_id}" ] || [ -z "${dedicated_id}" ]; then
  fail "a credential is empty"
fi
# One key on both sides would make every check below a test of the shared
# credential's own scope, whatever the Secret names say.
[ "${shared_id}" != "${dedicated_id}" ] ||
  fail "credential reuse: the shared and dedicated Secrets hold the same access key"

if ! mc alias set shared "${ENDPOINT}" "${shared_id}" \
  "$(cat "${credentials}/shared/SECRET_ACCESS_KEY")" --api S3v4 >/dev/null 2>"${mc_err}"; then
  redact "${mc_err}"
  fail "could not configure the shared credential"
fi
if ! mc alias set dedicated "${ENDPOINT}" "${dedicated_id}" \
  "$(cat "${credentials}/dedicated/SECRET_ACCESS_KEY")" --api S3v4 >/dev/null 2>"${mc_err}"; then
  redact "${mc_err}"
  fail "could not configure the dedicated credential"
fi
unset shared_id dedicated_id

# mc prints a success record compactly and an error record indented, so both
# spellings of the status field are recognised.
has_success() { grep -Eq '"status": ?"success"' "$1"; }
has_error() { grep -Eq '"status": ?"error"' "$1"; }

# 1. The dedicated credential reaches its own catalogue.
if ! mc ls --json "dedicated/${DEDICATED_BUCKET}/${DEDICATED_PREFIX}/" \
  >"${work}/dedicated-listing" 2>"${mc_err}" </dev/null; then
  redact "${mc_err}"
  fail "the dedicated credential cannot list its own catalogue, so a refusal would prove nothing"
fi
if has_error "${work}/dedicated-listing" || ! has_success "${work}/dedicated-listing"; then
  fail "the dedicated credential's own catalogue listing is empty or incomplete, so a refusal would prove nothing"
fi

# 2. The shared catalogue exists and holds the object the read is refused.
if ! mc ls --json --recursive "shared/${SHARED_BUCKET}/${SHARED_PREFIX}/" \
  >"${work}/shared-listing" 2>"${mc_err}" </dev/null; then
  redact "${mc_err}"
  fail "the shared credential cannot list the shared catalogue, so the refused targets are unproven"
fi
has_error "${work}/shared-listing" &&
  fail "the shared catalogue listing is incomplete, so the refused targets are unproven"
reference="$(grep -F '"type":"file"' "${work}/shared-listing" |
  sed -n 's/.*"key":"\([^"\\]*\)".*/\1/p' | head -n 1)" || reference=''
[ -n "${reference}" ] || fail "the shared catalogue holds no object, so the refused targets are unproven"
case "${reference}" in
  /* | *..*) fail "the shared catalogue named an unusable object key" ;;
esac
printf '%s' "${reference}" | grep -Eq '^[A-Za-z0-9._/-]+$' ||
  fail "the shared catalogue named an unusable object key"

# 3. Each access with the dedicated credential must be refused as AccessDenied.
#
# verdict <log> <exit-status> prints denied, granted or unproven. A success
# record wins over everything, because an accepted operation is a broken
# isolation whatever else the client printed.
verdict() {
  if has_success "$1"; then
    printf 'granted'
  elif grep -Eq '"Code": ?"AccessDenied"' "$1"; then
    printf 'denied'
  elif [ "$2" = 0 ]; then
    printf 'granted'
  else
    printf 'unproven'
  fi
}

# attempt <name> <command...> runs one access and records its verdict in
# <work>/<name>.verdict. The command's output is kept only in <work>/<name>.log.
attempt() {
  name="$1"
  shift
  status=0
  "$@" >"${work}/${name}.log" 2>&1 </dev/null || status=$?
  verdict "${work}/${name}.log" "${status}" >"${work}/${name}.verdict"
}

# codes <log> lists the S3 error codes the client reported, for the operator.
codes() {
  grep -Eo '"Code": ?"[A-Za-z]+"' "$1" | sed -e 's/.*"\([A-Za-z]*\)"$/\1/' | sort -u | tr '\n' ' '
}

attempt list mc ls --json "dedicated/${SHARED_BUCKET}/${SHARED_PREFIX}/"
attempt read mc cp --json "dedicated/${SHARED_BUCKET}/${SHARED_PREFIX}/${reference}" "${work}/read-probe"
# The read must never leave shared backup content behind in the pod.
rm -f "${work}/read-probe"
probe_key="wedding-backup-denial-probe/${PROBE_ID}"
printf 'Wedding backup denial probe %s\n' "${PROBE_ID}" >"${work}/write-probe"
attempt write mc cp --json "${work}/write-probe" "dedicated/${SHARED_BUCKET}/${probe_key}"

if [ "$(cat "${work}/write.verdict")" != denied ]; then
  # An accepted write must not stay in the shared bucket. Removing it uses the
  # shared credential, which owns that bucket.
  if ! mc rm --json "shared/${SHARED_BUCKET}/${probe_key}" >/dev/null 2>"${mc_err}" </dev/null; then
    redact "${mc_err}"
    printf 'denial-pod: could not remove %s/%s, which may not exist; check the shared bucket by hand\n' \
      "${SHARED_BUCKET}" "${probe_key}" >&2
  fi
fi

failed=false
for name in list read write; do
  case "$(cat "${work}/${name}.verdict")" in
    denied) ;;
    granted)
      printf 'denial-pod: ISOLATION BROKEN: the dedicated credential was allowed to %s the shared destination\n' \
        "${name}" >&2
      failed=true
      ;;
    *)
      printf 'denial-pod: the %s refusal is unproven: the client reported [ %s] rather than AccessDenied\n' \
        "${name}" "$(codes "${work}/${name}.log")" >&2
      redact "${work}/${name}.log"
      failed=true
      ;;
  esac
done
[ "${failed}" = false ] || fail "the shared destination did not refuse every access with AccessDenied"

printf '{"dedicatedCatalogueReachable":true,"sharedCatalogueReferenced":true,"listDenied":true,"readDenied":true,"writeDenied":true}\n'
printf '==== DENIAL OBSERVED ====\n'
