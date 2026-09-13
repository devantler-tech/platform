#!/usr/bin/env bash

# Pin the behaviour of the Wedding backup catalogue mirror:
# scripts/mirror-wedding-backup-catalogue.sh (runs on the runner) and
# scripts/mirror-wedding-backup-catalogue-pod.sh (runs in the cluster).
#
# WHY THIS EXISTS. The mirror is the only thing that fills the dedicated bucket
# before #3252 switches the database's archive reference. Its dangerous mistakes
# are quiet ones:
#
#   * reporting CONVERGED when the copy is incomplete, stale, or went to the wrong
#     place, so the cutover starts with no recoverable history;
#   * running at all after the Cluster has already switched, or while it switches;
#   * copying with one credential, deleting from the destination, or leaking the
#     account-specific endpoint into logs.
#
# The happy path runs the REAL evaluator on listings produced by the REAL pod
# script against a fake mc, so the three pieces are proven to agree on the listing
# format rather than each against its own idea of it. Needs Go, no cluster and no
# credentials.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly wrapper="${root_dir}/scripts/mirror-wedding-backup-catalogue.sh"
readonly pod_script="${root_dir}/scripts/mirror-wedding-backup-catalogue-pod.sh"

work_dir="$(mktemp -d)"
readonly work_dir
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

command -v go >/dev/null 2>&1 || {
  printf 'FAIL: go is required to build the real evaluator\n' >&2
  exit 1
}
readonly evaluator="${work_dir}/evaluator"
(cd "${root_dir}" && go build -o "${evaluator}" ./scripts/mirror-wedding-backup-catalogue)

readonly bin="${work_dir}/bin"
mkdir -p "${bin}"

cases_run=0

fail() {
  printf '\nFAIL: %s\n' "$1" >&2
  exit 1
}

require_text() {
  printf '%s' "$1" | grep -qF -- "$2" || fail "$3: expected '$2'. Got: $1"
}

refute_text() {
  if printf '%s' "$1" | grep -qF -- "$2"; then
    fail "$3: did not expect '$2'. Got: $1"
  fi
}

# ---------------------------------------------------------------------------
# Fake mc: `alias set`, `ls --json --recursive`, `cat`, `mirror`. Listings are
# served per alias and per call (ls-<alias>-<n>, falling back to ls-<alias>), so
# a case can make the source change between the starting and ending listings.
# ---------------------------------------------------------------------------
cat >"${bin}/mc" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
f="${FAKE_MC}"
case "$1" in
  alias)
    printf '%s %s\n' "$3" "$5" >>"${f}/aliases"
    exit 0
    ;;
  ls)
    target="${4%%/*}"
    count_file="${f}/ls-count-${target}"
    n=$(( $(cat "${count_file}" 2>/dev/null || echo 0) + 1 ))
    printf '%s' "${n}" >"${count_file}"
    [[ -e "${f}/ls-${target}-rc" ]] && exit "$(cat "${f}/ls-${target}-rc")"
    if [[ -e "${f}/ls-${target}-${n}" ]]; then cat "${f}/ls-${target}-${n}"; else cat "${f}/ls-${target}"; fi
    exit 0
    ;;
  cat)
    [[ -e "${f}/cat-rc" ]] && exit "$(cat "${f}/cat-rc")"
    printf 'bytes of %s' "${2##*/}"
    exit 0
    ;;
  mirror)
    printf '%s\n' "$*" >>"${f}/mirror.args"
    if [[ -e "${f}/mirror-rc" ]]; then
      printf 'mc: <ERROR> Failed to copy https://abc123.r2.cloudflarestorage.com/x\n' >&2
      exit "$(cat "${f}/mirror-rc")"
    fi
    exit 0
    ;;
esac
exit 64
FAKE
chmod +x "${bin}/mc"

readonly old='2026-09-01T00:00:00Z'
readonly base_id='20260908T000000'
readonly wal='000000010000000000000042'
readonly multipart_etag='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-3'

# record <key> <size> <etag>
record() {
  printf '{"status":"success","type":"file","lastModified":"%s","size":%s,"key":"%s","etag":"%s","url":"https://abc123.r2.cloudflarestorage.com","versionOrdinal":1,"storageClass":"STANDARD"}\n' \
    "${old}" "$2" "$1" "$3"
}

catalogue() {
  printf '{"status":"success","type":"folder","lastModified":"%s","size":0,"key":"wedding-db/","etag":"","url":"https://abc123.r2.cloudflarestorage.com"}\n' "${old}"
  record "wedding-db/base/${base_id}/backup.info" 120 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
  record "wedding-db/base/${base_id}/data.tar" 4096 "${multipart_etag}"
  record "wedding-db/wals/0000000100000000/${wal}.gz" 512 'cccccccccccccccccccccccccccccccc'
}

# new_pod_case <name>: fresh fake-mc fixtures and credentials. Echoes the dir.
new_pod_case() {
  local dir="${work_dir}/pod-$1"
  mkdir -p "${dir}/mc" "${dir}/credentials/source" "${dir}/credentials/destination" "${dir}/work"
  printf 'source-id' >"${dir}/credentials/source/ACCESS_KEY_ID"
  printf 'source-secret' >"${dir}/credentials/source/SECRET_ACCESS_KEY"
  printf 'destination-id' >"${dir}/credentials/destination/ACCESS_KEY_ID"
  printf 'destination-secret' >"${dir}/credentials/destination/SECRET_ACCESS_KEY"
  catalogue >"${dir}/mc/ls-source"
  catalogue >"${dir}/mc/ls-destination"
  printf '%s' "${dir}"
}

# run_pod <dir>: runs the pod script; sets pod_rc, pod_out, pod_err.
run_pod() {
  local dir="$1"
  pod_rc=0
  PATH="${bin}:${PATH}" FAKE_MC="${dir}/mc" CREDENTIALS_DIR="${dir}/credentials" \
    WORK_DIR="${dir}/work" ENDPOINT="${ENDPOINT_OVERRIDE:-https://abc123.r2.cloudflarestorage.com}" \
    SOURCE_BUCKET=platform-backups SOURCE_PREFIX=cnpg/wedding-db \
    DESTINATION_BUCKET=wedding-db-backups DESTINATION_PREFIX=cnpg/wedding-db \
    sh "${pod_script}" >"${dir}/out" 2>"${dir}/err" || pod_rc=$?
  pod_out="$(cat "${dir}/out")"
  pod_err="$(cat "${dir}/err")"
}

# --- pod script -------------------------------------------------------------

dir="$(new_pod_case happy)"
run_pod "${dir}"
[[ "${pod_rc}" == 0 ]] || fail "pod happy path exited ${pod_rc}: ${pod_err}"
expected_sha="$(printf 'bytes of data.tar' | shasum -a 256 | cut -d ' ' -f 1)"
require_text "${pod_out}" "{\"sha256\":\"${expected_sha}\"," 'multipart object carries the checksum of its bytes'
refute_text "${pod_out}" 'abc123.r2.cloudflarestorage.com' 'account-specific endpoint never reaches stdout'
require_text "${pod_out}" '"location":"platform-backups/cnpg/wedding-db"' 'source completion record names the source'
require_text "${pod_out}" '"location":"wedding-db-backups/cnpg/wedding-db"' 'destination completion record names the destination'
require_text "${pod_out}" '"files":3}' 'completion record counts files, not folders'
[[ "$(grep -c '"sha256"' "${dir}/out")" == 3 ]] || fail 'exactly the multipart object is checksummed on each of the three listings'
refute_text "$(cat "${dir}/mc/mirror.args")" '--remove' 'the copy never deletes from the destination'
require_text "$(cat "${dir}/mc/mirror.args")" 'source/platform-backups/cnpg/wedding-db/ destination/wedding-db-backups/cnpg/wedding-db/' 'copy reads source and writes destination'
require_text "$(cat "${dir}/mc/aliases")" 'source source-id' 'source alias uses the source credential'
require_text "$(cat "${dir}/mc/aliases")" 'destination destination-id' 'destination alias uses the destination credential'
readonly happy_pod_log="${dir}/out"
cases_run=$((cases_run + 1))

dir="$(new_pod_case reuse)"
printf 'source-id' >"${dir}/credentials/destination/ACCESS_KEY_ID"
run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must refuse one access key on both sides'
require_text "${pod_err}" 'credential reuse' 'reuse refusal names the reason'
[[ ! -e "${dir}/mc/mirror.args" ]] || fail 'no copy after a credential-reuse refusal'
cases_run=$((cases_run + 1))

dir="$(new_pod_case error-record)"
printf '{"status":"error","error":{"message":"Access Denied"}}\n' >>"${dir}/mc/ls-source"
run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must refuse a listing carrying an error record'
[[ ! -e "${dir}/mc/mirror.args" ]] || fail 'no copy after a refused starting listing'
cases_run=$((cases_run + 1))

dir="$(new_pod_case listing-fails)"
printf '1' >"${dir}/mc/ls-destination-rc"
run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must fail when a listing command fails'
refute_text "${pod_out}" 'listing-complete' 'no completion record is printed for a run that failed'
cases_run=$((cases_run + 1))

dir="$(new_pod_case checksum-read-fails)"
printf '1' >"${dir}/mc/cat-rc"
run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must fail when a multipart object cannot be read for its checksum'
require_text "${pod_err}" 'to checksum it' 'checksum read refusal names the reason'
cases_run=$((cases_run + 1))

dir="$(new_pod_case copy-fails)"
printf '1' >"${dir}/mc/mirror-rc"
run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must fail when the copy fails'
refute_text "${pod_err}" 'abc123' 'copy errors are redacted before they reach the log'
cases_run=$((cases_run + 1))

dir="$(new_pod_case plain-http)"
ENDPOINT_OVERRIDE='http://abc123.r2.cloudflarestorage.com' run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must refuse a non-https endpoint'
[[ ! -e "${dir}/mc/aliases" ]] || fail 'no credential is configured for a non-https endpoint'
cases_run=$((cases_run + 1))

# --- runner wrapper ---------------------------------------------------------

cat >"${bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
f="${FAKE_KUBE}"
while [[ "${1:-}" == --context || "${1:-}" == --namespace ]]; do shift 2; done
printf '%s\n' "$*" >>"${f}/calls"
case "$1 $2" in
  'get cluster.postgresql.cnpg.io')
    n=$(( $(cat "${f}/archive-count" 2>/dev/null || echo 0) + 1 ))
    printf '%s' "${n}" >"${f}/archive-count"
    if [[ -e "${f}/archive-${n}" ]]; then cat "${f}/archive-${n}"; else cat "${f}/archive"; fi
    ;;
  'get objectstores.barmancloud.cnpg.io')
    case "$5" in
      *destinationPath*) field=path ;;
      *endpointURL*) field=endpoint ;;
      *accessKeyId*) field=access ;;
      *secretAccessKey*) field=secret ;;
      *) exit 64 ;;
    esac
    cat "${f}/store-$3-${field}"
    ;;
  'create configmap') ;;
  'apply -f') cat >"${f}/applied.yaml" ;;
  'get pod') cat "${f}/phase" ;;
  'logs pod/'*) cat "${f}/pod.log" ;;
  'delete pod' | 'delete configmap') printf '%s %s\n' "$2" "$3" >>"${f}/deleted" ;;
  *) exit 64 ;;
esac
FAKE
chmod +x "${bin}/kubectl"

cat >"${bin}/fake-evaluator" <<'FAKE'
#!/usr/bin/env bash
f="${FAKE_KUBE}"
if [[ "$1" == evaluate ]]; then
  cat "${f}/summary" 2>/dev/null
  exit "$(cat "${f}/evaluate-rc" 2>/dev/null || echo 0)"
fi
exit 0
FAKE
chmod +x "${bin}/fake-evaluator"

# new_wrapper_case <name>: a live state that matches the reviewed plan.
new_wrapper_case() {
  local dir="${work_dir}/wrapper-$1"
  mkdir -p "${dir}"
  printf 'wedding-db' >"${dir}/archive"
  printf 's3://platform-backups/cnpg/wedding-db' >"${dir}/store-wedding-db-path"
  printf 's3://wedding-db-backups/cnpg/wedding-db' >"${dir}/store-wedding-db-dedicated-path"
  local store
  for store in wedding-db wedding-db-dedicated; do
    printf 'https://abc123.r2.cloudflarestorage.com' >"${dir}/store-${store}-endpoint"
  done
  printf 'wedding-db-backup-r2' >"${dir}/store-wedding-db-access"
  printf 'wedding-db-backup-r2' >"${dir}/store-wedding-db-secret"
  printf 'wedding-db-backup-r2-dedicated' >"${dir}/store-wedding-db-dedicated-access"
  printf 'wedding-db-backup-r2-dedicated' >"${dir}/store-wedding-db-dedicated-secret"
  printf 'Succeeded' >"${dir}/phase"
  cp "${happy_pod_log}" "${dir}/pod.log"
  printf '%s' "${dir}"
}

# run_wrapper <dir> [evaluator] [args...]: sets wrapper_rc, wrapper_out, wrapper_err.
run_wrapper() {
  local dir="$1" chosen="$2"
  shift 2
  wrapper_rc=0
  PATH="${bin}:${PATH}" FAKE_KUBE="${dir}" KUBECTL="${bin}/kubectl" MIRROR_EVALUATOR="${chosen}" \
    MIRROR_POLL_INTERVAL=0 MIRROR_POLL_LIMIT=3 GITHUB_RUN_ID=42 GITHUB_RUN_ATTEMPT=1 \
    bash "${wrapper}" "$@" >"${dir}/out" 2>"${dir}/err" || wrapper_rc=$?
  wrapper_out="$(cat "${dir}/out")"
  wrapper_err="$(cat "${dir}/err")"
}

dir="$(new_wrapper_case unconfirmed)"
run_wrapper "${dir}" "${evaluator}"
[[ "${wrapper_rc}" == 1 ]] || fail "an unconfirmed run must exit 1, got ${wrapper_rc}"
[[ ! -e "${dir}/calls" ]] || fail 'an unconfirmed run must not touch the cluster'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case converged)"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 0 ]] || fail "the real pod output must converge under the real evaluator, got ${wrapper_rc}: ${wrapper_err}"
require_text "${wrapper_out}" '"converged":true' 'the evaluator summary is printed'
require_text "${wrapper_out}" "\"sourceNewestBaseBackup\":\"wedding-db/${base_id}\"" 'the summary names the newest base backup'
require_text "${wrapper_out}" 'CONVERGED' 'the verdict is stated'
manifest="$(cat "${dir}/applied.yaml")"
require_text "${manifest}" 'secretName: wedding-db-backup-r2' 'the pod mounts the source credential'
require_text "${manifest}" 'secretName: wedding-db-backup-r2-dedicated' 'the pod mounts the destination credential'
require_text "${manifest}" '@sha256:7e3efb09c22c0882fbf341b9d99f61f94ae6c4c20a06f2f1a2b20ea8993d8952' 'the mc image is digest-pinned'
require_text "${manifest}" 'automountServiceAccountToken: false' 'the pod gets no Kubernetes API token'
require_text "${manifest}" 'readOnlyRootFilesystem: true' 'the pod root filesystem is read-only'
refute_text "${manifest}" '__' 'every manifest placeholder is substituted'
require_text "$(cat "${dir}/deleted")" 'pod wedding-backup-mirror-42-1' 'the pod is removed afterwards'
require_text "$(cat "${dir}/deleted")" 'configmap wedding-backup-mirror-42-1' 'the staged script is removed afterwards'
[[ "$(cat "${dir}/archive-count")" == 2 ]] || fail 'the archive reference is checked before and after the copy'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case already-switched)"
printf 'wedding-db-dedicated' >"${dir}/archive"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a switched Cluster must be refused, got ${wrapper_rc}"
[[ ! -e "${dir}/applied.yaml" ]] || fail 'no pod starts after the Cluster has switched'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case wrong-destination)"
printf 's3://platform-backups/cnpg/wedding-db-copy' >"${dir}/store-wedding-db-dedicated-path"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a drifted destination must be refused, got ${wrapper_rc}"
require_text "${wrapper_err}" 'wrong destination' 'the evaluator names the refusal'
[[ ! -e "${dir}/applied.yaml" ]] || fail 'no pod starts for a drifted destination'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case shared-secret)"
printf 'wedding-db-backup-r2' >"${dir}/store-wedding-db-dedicated-access"
printf 'wedding-db-backup-r2' >"${dir}/store-wedding-db-dedicated-secret"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "one Secret on both sides must be refused, got ${wrapper_rc}"
[[ ! -e "${dir}/applied.yaml" ]] || fail 'no pod starts when both stores share a Secret'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case switched-mid-run)"
printf 'wedding-db-dedicated' >"${dir}/archive-2"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a Cluster that switched during the copy must be refused, got ${wrapper_rc}"
refute_text "${wrapper_out}" 'CONVERGED' 'no verdict is reported once the reference moved'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case pod-failed)"
printf 'Failed' >"${dir}/phase"
printf 'mirror-pod: the copy did not complete\n' >"${dir}/pod.log"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a failed pod must exit 1, got ${wrapper_rc}"
require_text "${wrapper_err}" 'the copy did not complete' 'the pod refusal reason is surfaced'
require_text "$(cat "${dir}/deleted")" 'pod wedding-backup-mirror-42-1' 'a failed pod is still removed'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case pod-never-finishes)"
printf 'Running' >"${dir}/phase"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a pod that outlives the bound must exit 1, got ${wrapper_rc}"
refute_text "${wrapper_out}" 'CONVERGED' 'an unfinished pod yields no verdict'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case truncated-log)"
sed '/==== BEGIN destination ====/,$d' "${happy_pod_log}" >"${dir}/pod.log"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a log missing the destination listing must be refused, got ${wrapper_rc}"
require_text "${wrapper_err}" 'destination listing' 'the missing section is named'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case partial-copy)"
grep -v "${wal}" "${happy_pod_log}" | sed 's/"files":3}/"files":2}/' >"${dir}/partial.log"
# Only the destination listing loses the WAL segment.
awk -v wal="${wal}" '
  /==== BEGIN destination ====/ { dest = 1 }
  /==== END destination ====/ { dest = 0 }
  dest && index($0, wal) { next }
  dest && /"type":"listing-complete"/ { sub(/"files":3}/, "\"files\":2}") }
  { print }
' "${happy_pod_log}" >"${dir}/pod.log"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a destination missing an object must be refused, got ${wrapper_rc}"
require_text "${wrapper_err}" 'refused' 'the refusal is reported'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case not-converged)"
printf '{"converged":false,"pendingObjects":1}\n' >"${dir}/summary"
run_wrapper "${dir}" "${bin}/fake-evaluator" --confirm
[[ "${wrapper_rc}" == 3 ]] || fail "a verified but unconverged mirror must exit 3, got ${wrapper_rc}"
require_text "${wrapper_out}" 'Run the mirror again' 'the operator is told to re-run'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case no-verdict)"
printf '{"pendingObjects":0}\n' >"${dir}/summary"
run_wrapper "${dir}" "${bin}/fake-evaluator" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a summary without a verdict must exit 1, got ${wrapper_rc}"
cases_run=$((cases_run + 1))

printf 'test-mirror-wedding-backup-catalogue: %d cases passed\n' "${cases_run}"
