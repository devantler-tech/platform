#!/bin/sh
# Prove, from a platform-owned pod, that the dedicated Wedding backup
# credential is refused by the shared backup destination.
#
# Runs in the pod that scripts/verify-wedding-backup-denial.sh creates. It is
# delivered as a ConfigMap and executed by the digest-pinned mc image with a
# static BusyBox toolbox from an init container. It is
# POSIX sh and uses only the tools checked below.
#
# WHAT IT PROVES, refusing at the first precondition it cannot prove:
#   1. The dedicated credential lists its own catalogue. A refusal observed later
#      therefore cannot come from a broken key, endpoint or network path.
#   2. The shared credential lists the shared catalogue and names one object in
#      it. The refused targets therefore exist, so a mistyped bucket or an empty
#      prefix cannot pass as a denial.
#   3. With the dedicated credential, listing the shared catalogue, reading that
#      object and writing a probe object to the shared bucket are each refused.
#      All three are attempted and reported. An access counts as refused only
#      when mc exits non-zero, reports S3's AccessDenied, and left no trace: no
#      listed entry, no local copy of the object, and no probe object in the
#      shared bucket when the shared credential lists it afterwards. A trace or a
#      successful exit means the isolation is broken. Any other error is not
#      evidence of denial, so it fails the proof as unproven.
#
# WHAT IT NEVER DOES. It never prints a credential or the account-specific
# endpoint host, and it writes nothing except the probe object the dedicated
# credential must be refused. If that write lands anyway, the shared credential
# removes the probe object before the proof fails.
#
# OUTPUT. On success, one receipt line followed by `==== DENIAL OBSERVED ====`.
# Every refusal, with redacted client output where it helps, is a `denial-pod: `
# line on stderr, and the script exits non-zero.

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

printf '%s' "${ENDPOINT}" | grep -Eq '^https://[a-z0-9][a-z0-9.-]*$' ||
  fail "the R2 endpoint must be an https URL with a bare host"
for bucket in "${SHARED_BUCKET}" "${DEDICATED_BUCKET}"; do
  printf '%s' "${bucket}" | grep -Eq '^[a-z0-9][a-z0-9.-]*$' || fail "a bucket name is not valid"
done
[ "${SHARED_BUCKET}" != "${DEDICATED_BUCKET}" ] ||
  fail "the shared and dedicated destinations are the same bucket"
printf '%s' "${PROBE_ID}" | grep -Eq '^[a-z0-9-]{1,40}$' || fail "the probe id is not a valid name fragment"

credentials="${CREDENTIALS_DIR:-/credentials}"
work="${WORK_DIR:-/tmp/denial}"
mkdir -p "${work}"
export MC_CONFIG_DIR="${MC_CONFIG_DIR:-${work}/.mc}"
mc_err="${work}/mc.err"
host="${ENDPOINT#https://}"
readonly probe_prefix='wedding-backup-denial-probe'
probe_key="${probe_prefix}/${PROBE_ID}"

# redact prints the last lines of an mc log as `denial-pod: ` lines, without the
# endpoint host (with or without its scheme), any IPv4 address such as a pod or
# cluster DNS address, or any long hex token such as an access key ID or request
# ID. These lines reach a public workflow log.
redact() {
  sed -e 's#https\{0,1\}://[^/ "`]*#<endpoint>#g' -e "s#${host}#<endpoint>#g" \
    -e 's/[0-9a-fA-F]\{32,\}/<hex>/g' \
    -e 's/<hex>\.[0-9.:]*/<hex><ip>/g' \
    -e 's/[0-9]\{1,3\}\(\.[0-9]\{1,3\}\)\{3\}\(:[0-9]\{1,5\}\)\{0,1\}/<ip>/g' "$1" | tail -n 5 | sed -e 's/^/denial-pod:   /' >&2
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

# mc prints its JSON records compactly or indented depending on the command and
# terminal, so both spellings of the status field are recognised.
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

# probe_state prints present, absent or unknown: whether the shared credential
# sees the probe prefix at the top level of the shared bucket. The top level is
# never empty (the shared catalogue lives there), so an empty or failed listing
# is unknown rather than absent.
probe_state() {
  if ! mc ls --json "shared/${SHARED_BUCKET}/" >"${work}/top-level" 2>"${mc_err}" </dev/null ||
    has_error "${work}/top-level" || ! has_success "${work}/top-level"; then
    printf unknown
  elif grep -Fq "\"key\":\"${probe_prefix}/\"" "${work}/top-level"; then
    printf present
  else
    printf absent
  fi
}

# The write is judged by whether its object appears, so the prefix must be
# absent before it. A leftover can only come from an earlier accepted write.
case "$(probe_state)" in
  absent) ;;
  present) fail "the shared bucket already holds ${probe_prefix}/ from an earlier run; inspect and remove it before proving again" ;;
  *)
    redact "${mc_err}"
    fail "the shared credential cannot list the shared bucket, so the write would be unobserved"
    ;;
esac

# 3. Each access with the dedicated credential must be refused.

# denial_reported <log> succeeds when mc reported S3's AccessDenied. The pinned
# client reports it as the error code for a listing, but as its own
# insufficient-permissions error, with no code, for a copy; that error only
# counts when it names a path in the shared bucket, never a local one.
denial_reported() {
  if grep -Eq '"Code": ?"AccessDenied"' "$1"; then
    return 0
  fi
  grep -E "Insufficient permissions to access this path.*/${SHARED_BUCKET}/" "$1" >/dev/null
}

# attempt <name> <command...> runs one access and records its exit status in
# <work>/<name>.status. Its output stays in <work>/<name>.log.
attempt() {
  name="$1"
  shift
  status=0
  "$@" >"${work}/${name}.log" 2>&1 </dev/null || status=$?
  printf '%s' "${status}" >"${work}/${name}.status"
}

# refusal <name> prints granted, denied or unproven for an access that left no
# trace. A successful exit is an accepted access whatever else mc printed.
refusal() {
  if [ "$(cat "${work}/$1.status")" = 0 ]; then
    printf granted
  elif denial_reported "${work}/$1.log"; then
    printf denied
  else
    printf unproven
  fi
}

attempt list mc ls --json "dedicated/${SHARED_BUCKET}/${SHARED_PREFIX}/"
# A listed entry is shared catalogue content the dedicated credential could see.
if has_success "${work}/list.log"; then
  printf granted >"${work}/list.verdict"
else
  refusal list >"${work}/list.verdict"
fi

attempt read mc cp --json "dedicated/${SHARED_BUCKET}/${SHARED_PREFIX}/${reference}" "${work}/read-probe"
# A local copy is shared backup content the dedicated credential could read. It
# never stays in the pod.
if [ -e "${work}/read-probe" ]; then
  rm -f "${work}/read-probe"
  printf granted >"${work}/read.verdict"
else
  refusal read >"${work}/read.verdict"
fi

printf 'Wedding backup denial probe %s\n' "${PROBE_ID}" >"${work}/write-probe"
# mc reports an upload as successful before the destination answers it, so a
# success record says nothing about the write; where the object ended up does.
attempt write mc cp --json "${work}/write-probe" "dedicated/${SHARED_BUCKET}/${probe_key}"
# The shared credential, which owns the bucket, decides whether the write landed.
case "$(probe_state)" in
  absent) refusal write >"${work}/write.verdict" ;;
  present) printf granted >"${work}/write.verdict" ;;
  *)
    redact "${mc_err}"
    printf 'denial-pod: the shared credential cannot list the shared bucket after the write, so its outcome is unobserved\n' >&2
    printf unobserved >"${work}/write.verdict"
    ;;
esac

if [ "$(cat "${work}/write.verdict")" != denied ]; then
  # A landed write must not stay in the shared bucket.
  if ! mc rm --json "shared/${SHARED_BUCKET}/${probe_key}" >/dev/null 2>"${mc_err}" </dev/null; then
    redact "${mc_err}"
    printf 'denial-pod: could not remove %s/%s, which may not exist; check the shared bucket by hand\n' \
      "${SHARED_BUCKET}" "${probe_key}" >&2
  fi
fi

# codes <log> lists the S3 error codes mc reported, for the operator.
codes() {
  grep -Eo '"Code": ?"[A-Za-z]+"' "$1" | sed -e 's/.*"\([A-Za-z]*\)"$/\1/' | sort -u | tr '\n' ' '
}

failed=false
for name in list read write; do
  case "$(cat "${work}/${name}.verdict")" in
    denied) ;;
    granted)
      printf 'denial-pod: ISOLATION BROKEN: the dedicated credential was allowed to %s the shared destination\n' \
        "${name}" >&2
      failed=true
      ;;
    unobserved) failed=true ;;
    *)
      printf 'denial-pod: the %s refusal is unproven: mc exited %s and reported [ %s] rather than AccessDenied\n' \
        "${name}" "$(cat "${work}/${name}.status")" "$(codes "${work}/${name}.log")" >&2
      redact "${work}/${name}.log"
      failed=true
      ;;
  esac
done
[ "${failed}" = false ] || fail "the shared destination did not refuse every access"

printf '{"dedicatedCatalogueReachable":true,"sharedCatalogueReferenced":true,"listDenied":true,"readDenied":true,"writeDenied":true}\n'
printf '==== DENIAL OBSERVED ====\n'
