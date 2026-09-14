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
#   * refusing forever on a correct copy, so the cutover can never be proven;
#   * running at all after the Cluster has already switched, or while it switches;
#   * copying with one credential, deleting from the destination, or leaking the
#     account-specific endpoint.
#
# The converging cases run the REAL evaluator on listings produced by the REAL pod
# script against a fake mc, so the pieces are proven to agree on the listing
# format rather than each against its own idea of it. Needs Go, no cluster and no
# credentials.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly wrapper="${root_dir}/scripts/mirror-wedding-backup-catalogue.sh"
readonly pod_script="${root_dir}/scripts/mirror-wedding-backup-catalogue-pod.sh"

work_dir="$(mktemp -d)"
readonly work_dir
# shellcheck disable=SC2317,SC2329 # Invoked indirectly by the EXIT trap.
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
# Every failure writes the endpoint host to stderr, so redaction is exercised.
# ---------------------------------------------------------------------------
cat >"${bin}/mc" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
f="${FAKE_MC}"
leak() {
  printf 'mc: <ERROR> request to https://abc123.r2.cloudflarestorage.com/x failed\n' >&2
}
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
    if [[ -e "${f}/ls-${target}-rc" ]]; then leak; exit "$(cat "${f}/ls-${target}-rc")"; fi
    if [[ -e "${f}/ls-${target}-${n}" ]]; then cat "${f}/ls-${target}-${n}"; else cat "${f}/ls-${target}"; fi
    exit 0
    ;;
  cat)
    printf '%s\n' "$2" >>"${f}/catted"
    if [[ -e "${f}/cat-rc" ]]; then leak; exit "$(cat "${f}/cat-rc")"; fi
    printf 'bytes of %s' "${2##*/}"
    exit 0
    ;;
  mirror)
    printf '%s\n' "$*" >>"${f}/mirror.args"
    if [[ -e "${f}/mirror-rc" ]]; then leak; exit "$(cat "${f}/mirror-rc")"; fi
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
readonly single_etag='dddddddddddddddddddddddddddddddd'

# record <key> <size> <etag>
record() {
  printf '{"status":"success","type":"file","lastModified":"%s","size":%s,"key":"%s","etag":"%s","url":"https://abc123.r2.cloudflarestorage.com","versionOrdinal":1,"storageClass":"STANDARD"}\n' \
    "${old}" "$2" "$1" "$3"
}

# catalogue [data-etag]
catalogue() {
  printf '{"status":"success","type":"folder","lastModified":"%s","size":0,"key":"wedding-db/","etag":"","url":"https://abc123.r2.cloudflarestorage.com"}\n' "${old}"
  record "wedding-db/base/${base_id}/backup.info" 120 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
  record "wedding-db/base/${base_id}/data.tar" 4096 "${1:-${multipart_etag}}"
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

# run_pod <dir>: runs the pod script; sets pod_rc, pod_out, pod_err, pod_files.
run_pod() {
  local dir="$1"
  pod_rc=0
  PATH="${bin}:${PATH}" FAKE_MC="${dir}/mc" CREDENTIALS_DIR="${dir}/credentials" \
    WORK_DIR="${dir}/work" COLLECT_TIMEOUT=0 \
    ENDPOINT="${ENDPOINT_OVERRIDE:-https://abc123.r2.cloudflarestorage.com}" \
    SOURCE_BUCKET=platform-backups SOURCE_PREFIX=cnpg/wedding-db \
    DESTINATION_BUCKET=wedding-db-backups DESTINATION_PREFIX=cnpg/wedding-db \
    sh "${pod_script}" >"${dir}/out" 2>"${dir}/err" || pod_rc=$?
  pod_out="$(cat "${dir}/out")"
  pod_err="$(cat "${dir}/err")"
  pod_files="$(cat "${dir}/work/source-before" "${dir}/work/source-after" "${dir}/work/destination" 2>/dev/null || true)"
}

expected_sha="$(printf 'bytes of data.tar' | shasum -a 256 | cut -d ' ' -f 1)"
readonly expected_sha

# --- pod script -------------------------------------------------------------

dir="$(new_pod_case happy)"
run_pod "${dir}"
[[ "${pod_rc}" == 0 ]] || fail "pod happy path exited ${pod_rc}: ${pod_err}"
require_text "${pod_out}" '==== LISTINGS READY ====' 'the pod announces its listings'
require_text "${pod_files}" "{\"sha256\":\"${expected_sha}\"," 'the multipart object carries the checksum of its bytes'
refute_text "${pod_files}${pod_out}" 'abc123.r2.cloudflarestorage.com' 'the account-specific endpoint never reaches the listings or stdout'
require_text "${pod_files}" '"location":"platform-backups/cnpg/wedding-db"' 'source completion records name the source'
require_text "${pod_files}" '"location":"wedding-db-backups/cnpg/wedding-db"' 'the destination completion record names the destination'
[[ "$(grep -c '"files":3}' <<<"${pod_files}")" == 3 ]] || fail 'every completion record counts files, not folders'
[[ "$(grep -c '"sha256"' <<<"${pod_files}")" == 3 ]] || fail 'exactly the multipart object is checksummed on each of the three listings'
[[ "$(grep -c "source/platform-backups/cnpg/wedding-db/wedding-db/base/${base_id}/data.tar" "${dir}/mc/catted")" == 1 ]] ||
  fail 'the source copy of a multipart object is read once, not once per listing'
[[ "$(head -c 20 "${dir}/work/run-start")" == "$(sed -n '$s/.*"started":"\([^"]*\)".*/\1/p' "${dir}/work/source-before")" ]] ||
  fail 'the run start is the starting listing start'
refute_text "$(cat "${dir}/mc/mirror.args")" '--remove' 'the copy never deletes from the destination'
require_text "$(cat "${dir}/mc/mirror.args")" 'source/platform-backups/cnpg/wedding-db/ destination/wedding-db-backups/cnpg/wedding-db/' 'the copy reads the source and writes the destination'
require_text "$(cat "${dir}/mc/aliases")" 'source source-id' 'the source alias uses the source credential'
require_text "$(cat "${dir}/mc/aliases")" 'destination destination-id' 'the destination alias uses the destination credential'
readonly happy_work="${dir}/work"
cases_run=$((cases_run + 1))

# The copy is uploaded single-part although the original was multipart. Both
# sides must still carry a checksum, or the evaluator can never verify the object.
dir="$(new_pod_case one-sided-multipart)"
catalogue "${single_etag}" >"${dir}/mc/ls-destination"
run_pod "${dir}"
[[ "${pod_rc}" == 0 ]] || fail "pod one-sided multipart exited ${pod_rc}: ${pod_err}"
destination_record="$(grep 'data.tar' "${dir}/work/destination" || true)"
require_text "${destination_record}" "{\"sha256\":\"${expected_sha}\"," 'a single-part copy of a multipart original is checksummed too'
readonly one_sided_work="${dir}/work"
cases_run=$((cases_run + 1))

# No multipart object anywhere: nothing is checksummed, and the listings must
# come through whole rather than be consumed while no checksums exist.
dir="$(new_pod_case no-multipart)"
catalogue "${single_etag}" >"${dir}/mc/ls-source"
catalogue "${single_etag}" >"${dir}/mc/ls-destination"
run_pod "${dir}"
[[ "${pod_rc}" == 0 ]] || fail "pod without multipart objects exited ${pod_rc}: ${pod_err}"
refute_text "${pod_files}" '"sha256"' 'nothing is checksummed when no object is multipart'
[[ "$(grep -c 'data.tar' <<<"${pod_files}")" == 3 ]] || fail 'every listing keeps its objects when no checksum is added'
[[ ! -e "${dir}/mc/catted" ]] || fail 'no object is downloaded when nothing needs a checksum'
readonly no_multipart_work="${dir}/work"
cases_run=$((cases_run + 1))

dir="$(new_pod_case reuse)"
printf 'source-id' >"${dir}/credentials/destination/ACCESS_KEY_ID"
run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must refuse one access key on both sides'
require_text "${pod_err}" 'credential reuse' 'the reuse refusal names the reason'
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
refute_text "${pod_out}" 'LISTINGS READY' 'a failed run never announces its listings'
refute_text "${pod_err}" 'abc123' 'listing errors are redacted before they reach the log'
cases_run=$((cases_run + 1))

dir="$(new_pod_case checksum-read-fails)"
printf '1' >"${dir}/mc/cat-rc"
run_pod "${dir}"
[[ "${pod_rc}" != 0 ]] || fail 'pod must fail when a multipart object cannot be read for its checksum'
require_text "${pod_err}" 'to checksum it' 'the checksum read refusal names the reason'
refute_text "${pod_err}" 'abc123' 'checksum read errors are redacted'
refute_text "${pod_out}" 'LISTINGS READY' 'a failed checksum never announces the listings'
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
      *accessKeyId.name*) field=access ;;
      *secretAccessKey.name*) field=secret ;;
      *accessKeyId.key*) field=access-key; default=ACCESS_KEY_ID ;;
      *secretAccessKey.key*) field=secret-key; default=SECRET_ACCESS_KEY ;;
      *) exit 64 ;;
    esac
    if [[ -e "${f}/store-$3-${field}" ]]; then cat "${f}/store-$3-${field}"; else printf '%s' "${default:?}"; fi
    ;;
  'create configmap') ;;
  'apply -f') cat >"${f}/applied.yaml" ;;
  'get pod') cat "${f}/phase" ;;
  'logs pod/'*) cat "${f}/pod.log" ;;
  'exec '*)
    # exec <name> -c mirror -- cat /work/<listing>
    cat "${f}/work/${7##*/}"
    ;;
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

# new_wrapper_case <name> [pod-work-dir]: a live state that matches the reviewed plan.
new_wrapper_case() {
  local dir="${work_dir}/wrapper-$1"
  mkdir -p "${dir}/work"
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
  printf 'Running' >"${dir}/phase"
  printf '==== LISTINGS READY ====\n' >"${dir}/pod.log"
  printf 'apiVersion: v1\nkind: ConfigMap\ndata:\n  r2_bucket: platform-backups\n  r2_endpoint: https://abc123.r2.cloudflarestorage.com\n' >"${dir}/bootstrap.yaml"
  cp "${2:-${happy_work}}"/run-start "${2:-${happy_work}}"/source-before \
    "${2:-${happy_work}}"/source-after "${2:-${happy_work}}"/destination "${dir}/work/"
  printf '%s' "${dir}"
}

# run_wrapper <dir> <evaluator> [args...]: sets wrapper_rc, wrapper_out, wrapper_err.
run_wrapper() {
  local dir="$1" chosen="$2"
  shift 2
  wrapper_rc=0
  PATH="${bin}:${PATH}" FAKE_KUBE="${dir}" KUBECTL="${bin}/kubectl" MIRROR_EVALUATOR="${chosen}" \
    MIRROR_BOOTSTRAP_CONFIG="${dir}/bootstrap.yaml" \
    MIRROR_POLL_INTERVAL=0 MIRROR_POLL_LIMIT=3 GITHUB_RUN_ID=42 GITHUB_RUN_ATTEMPT=1 \
    bash "${wrapper}" "$@" >"${dir}/out" 2>"${dir}/err" || wrapper_rc=$?
  wrapper_out="$(cat "${dir}/out")"
  wrapper_err="$(cat "${dir}/err")"
}

# refuse_before_pod <dir> <what>: the run exits 1 without creating anything.
refuse_before_pod() {
  [[ "${wrapper_rc}" == 1 ]] || fail "$2 must exit 1, got ${wrapper_rc}: ${wrapper_err}"
  [[ ! -e "$1/applied.yaml" ]] || fail "$2 must not start a pod"
}

dir="$(new_wrapper_case unconfirmed)"
run_wrapper "${dir}" "${evaluator}"
[[ "${wrapper_rc}" == 1 ]] || fail "an unconfirmed run must exit 1, got ${wrapper_rc}"
[[ ! -e "${dir}/calls" ]] || fail 'an unconfirmed run must not touch the cluster'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case converged)"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 0 ]] || fail "the real pod listings must converge under the real evaluator, got ${wrapper_rc}: ${wrapper_err}"
require_text "${wrapper_out}" '"converged":true' 'the evaluator summary is printed'
require_text "${wrapper_out}" "\"sourceNewestBaseBackup\":\"wedding-db/${base_id}\"" 'the summary names the newest base backup'
require_text "${wrapper_out}" 'CONVERGED' 'the verdict is stated'
manifest="$(cat "${dir}/applied.yaml")"
require_text "${manifest}" 'secretName: wedding-db-backup-r2' 'the pod mounts the source credential'
require_text "${manifest}" 'secretName: wedding-db-backup-r2-dedicated' 'the pod mounts the destination credential'
require_text "${manifest}" '@sha256:7e3efb09c22c0882fbf341b9d99f61f94ae6c4c20a06f2f1a2b20ea8993d8952' 'the mc image is digest-pinned'
require_text "${manifest}" 'value: "https://abc123.r2.cloudflarestorage.com"' 'the endpoint is substituted literally'
require_text "${manifest}" 'automountServiceAccountToken: false' 'the pod gets no Kubernetes API token'
require_text "${manifest}" 'readOnlyRootFilesystem: true' 'the pod root filesystem is read-only'
require_text "${manifest}" 'medium: Memory' 'the mc credential config lives in memory'
require_text "${manifest}" 'value: /mc-config' 'mc is pointed at the memory-backed config directory'
require_text "${wrapper_out}" 'catch-up' 'the verdict says the cutover still needs a catch-up'
refute_text "${manifest}" '__' 'every manifest placeholder is substituted'
require_text "$(cat "${dir}/deleted")" 'pod wedding-backup-mirror-42-1' 'the pod is removed afterwards'
require_text "$(cat "${dir}/deleted")" 'configmap wedding-backup-mirror-42-1' 'the staged script is removed afterwards'
[[ "$(cat "${dir}/archive-count")" == 2 ]] || fail 'the archive reference is checked before and after the copy'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case one-sided-multipart "${one_sided_work}")"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 0 ]] || fail "a single-part copy of a multipart original must still converge, got ${wrapper_rc}: ${wrapper_err}"
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case no-multipart "${no_multipart_work}")"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 0 ]] || fail "a catalogue without multipart objects must converge, got ${wrapper_rc}: ${wrapper_err}"
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case already-switched)"
printf 'wedding-db-dedicated' >"${dir}/archive"
run_wrapper "${dir}" "${evaluator}" --confirm
refuse_before_pod "${dir}" 'a switched Cluster'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case wrong-destination)"
printf 's3://platform-backups/cnpg/wedding-db-copy' >"${dir}/store-wedding-db-dedicated-path"
run_wrapper "${dir}" "${evaluator}" --confirm
refuse_before_pod "${dir}" 'a drifted destination'
require_text "${wrapper_err}" 'wrong destination' 'the evaluator names the refusal'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case shared-secret)"
printf 'wedding-db-backup-r2' >"${dir}/store-wedding-db-dedicated-access"
printf 'wedding-db-backup-r2' >"${dir}/store-wedding-db-dedicated-secret"
run_wrapper "${dir}" "${evaluator}" --confirm
refuse_before_pod "${dir}" 'one Secret on both sides'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case renamed-key)"
printf 'AWS_ACCESS_KEY_ID' >"${dir}/store-wedding-db-dedicated-access-key"
run_wrapper "${dir}" "${evaluator}" --confirm
refuse_before_pod "${dir}" 'a credential key the pod does not mount'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case injected-endpoint)"
printf 'https://abc123.r2.cloudflarestorage.com"\n        - name: X' >"${dir}/store-wedding-db-endpoint"
printf 'https://abc123.r2.cloudflarestorage.com"\n        - name: X' >"${dir}/store-wedding-db-dedicated-endpoint"
run_wrapper "${dir}" "${evaluator}" --confirm
refuse_before_pod "${dir}" 'an endpoint that could inject manifest fields'
cases_run=$((cases_run + 1))

# Both stores agree on a well-formed host that tenant egress would even allow, but
# it is not the committed endpoint, so neither credential may be sent to it.
dir="$(new_wrapper_case foreign-endpoint)"
printf 'https://attacker.r2.cloudflarestorage.com' >"${dir}/store-wedding-db-endpoint"
printf 'https://attacker.r2.cloudflarestorage.com' >"${dir}/store-wedding-db-dedicated-endpoint"
run_wrapper "${dir}" "${evaluator}" --confirm
refuse_before_pod "${dir}" 'an endpoint other than the committed R2 endpoint'
require_text "${wrapper_err}" 'committed R2 endpoint' 'the pin names the reason'
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

dir="$(new_wrapper_case pod-never-ready)"
: >"${dir}/pod.log"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a pod that never reports ready within the bound must exit 1, got ${wrapper_rc}"
refute_text "${wrapper_out}" 'CONVERGED' 'an unfinished pod yields no verdict'
grep -q '^exec ' "${dir}/calls" && fail 'nothing is collected from a pod that never reported ready'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case listing-not-collectable)"
rm "${dir}/work/destination"
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a listing that cannot be collected must be refused, got ${wrapper_rc}"
require_text "${wrapper_err}" 'destination listing' 'the missing listing is named'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case partial-copy)"
# The destination listing loses the WAL segment and is otherwise complete.
grep -v "${wal}" "${happy_work}/destination" | sed 's/"files":3}/"files":2}/' >"${dir}/work/destination"
grep -q '"files":2}' "${dir}/work/destination" || fail 'the partial-copy fixture was not built'
run_wrapper "${dir}" "${evaluator}" --confirm
[[ "${wrapper_rc}" == 1 ]] || fail "a destination missing an object must be refused, got ${wrapper_rc}"
require_text "${wrapper_err}" 'partial copy' 'the evaluator refuses it as a partial copy'
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

# --- catch-up after the switch (#3778) ---------------------------------------

# The pod's catalogue was archived on ${old}; the switch was recorded after that
# and before the catch-up's own listings start.
readonly switch_time='2026-09-10T00:00:00Z'
readonly next_wal='000000010000000000000043'
readonly gap_wal='000000010000000000000045'

# with_post_switch_segment <dir> <segment> <lastModified>: the destination listing
# gains one segment archived through the dedicated store.
with_post_switch_segment() {
  local listing="$1/work/destination"
  {
    grep -v '"type":"listing-complete"' "${happy_work}/destination"
    printf '{"status":"success","type":"file","lastModified":"%s","size":512,"key":"wedding-db/wals/0000000100000000/%s.gz","etag":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}\n' "$3" "$2"
    grep '"type":"listing-complete"' "${happy_work}/destination" | sed 's/"files":3}/"files":4}/'
  } >"${listing}"
  grep -q '"files":4}' "${listing}" || fail 'the post-switch fixture was not built'
}

dir="$(new_wrapper_case catch-up-caught-up)"
printf 'wedding-db-dedicated' >"${dir}/archive"
with_post_switch_segment "${dir}" "${next_wal}" '2026-09-12T00:00:00Z'
run_wrapper "${dir}" "${evaluator}" --confirm --catch-up "${switch_time}"
[[ "${wrapper_rc}" == 0 ]] || fail "a complete catch-up with continuous WAL must exit 0, got ${wrapper_rc}: ${wrapper_err}"
require_text "${wrapper_out}" '"converged":true' 'the catch-up summary is printed'
require_text "${wrapper_out}" "\"oldestPostSwitchWal\":\"${next_wal}\"" 'the summary names the first post-switch segment'
require_text "${wrapper_out}" 'CAUGHT UP' 'the catch-up verdict is stated'
refute_text "${wrapper_out}" 'NOT CAUGHT UP' 'a caught-up pass is not reported as pending'
[[ "$(cat "${dir}/archive-count")" == 2 ]] || fail 'the catch-up checks the archive reference before and after the copy'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case catch-up-waiting)"
printf 'wedding-db-dedicated' >"${dir}/archive"
run_wrapper "${dir}" "${evaluator}" --confirm --catch-up "${switch_time}"
[[ "${wrapper_rc}" == 3 ]] || fail "a catch-up with no post-switch segment yet must exit 3, got ${wrapper_rc}: ${wrapper_err}"
require_text "${wrapper_out}" 'NOT CAUGHT UP' 'the operator is told the catch-up is not complete'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case catch-up-before-cutover)"
run_wrapper "${dir}" "${evaluator}" --confirm --catch-up "${switch_time}"
refuse_before_pod "${dir}" 'a catch-up while the Cluster still archives to the shared store'
require_text "${wrapper_err}" 'only runs after the cutover' 'the refusal names the reason'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case catch-up-switched-back)"
printf 'wedding-db-dedicated' >"${dir}/archive"
printf 'wedding-db' >"${dir}/archive-2"
run_wrapper "${dir}" "${evaluator}" --confirm --catch-up "${switch_time}"
[[ "${wrapper_rc}" == 1 ]] || fail "a Cluster that switched back during the catch-up must be refused, got ${wrapper_rc}"
refute_text "${wrapper_out}" 'CAUGHT UP' 'no verdict is reported once the reference moved'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case catch-up-gap)"
printf 'wedding-db-dedicated' >"${dir}/archive"
with_post_switch_segment "${dir}" "${gap_wal}" '2026-09-12T00:00:00Z'
run_wrapper "${dir}" "${evaluator}" --confirm --catch-up "${switch_time}"
[[ "${wrapper_rc}" == 1 ]] || fail "a WAL gap across the switch must be refused, got ${wrapper_rc}"
require_text "${wrapper_err}" 'WAL gap' 'the evaluator names the gap'
cases_run=$((cases_run + 1))

dir="$(new_wrapper_case catch-up-stale-object)"
printf 'wedding-db-dedicated' >"${dir}/archive"
with_post_switch_segment "${dir}" "${next_wal}" '2026-09-05T00:00:00Z'
run_wrapper "${dir}" "${evaluator}" --confirm --catch-up "${switch_time}"
[[ "${wrapper_rc}" == 1 ]] || fail "a destination object written before the switch must be refused, got ${wrapper_rc}"
require_text "${wrapper_err}" 'unexpected destination object' 'the evaluator names the stale object'
cases_run=$((cases_run + 1))

for bad in 'yesterday' '2026-09-10' '2999-01-01T00:00:00Z'; do
  dir="$(new_wrapper_case "catch-up-bad-time-${bad//[^a-z0-9]/-}")"
  printf 'wedding-db-dedicated' >"${dir}/archive"
  run_wrapper "${dir}" "${evaluator}" --confirm --catch-up "${bad}"
  [[ "${wrapper_rc}" == 1 ]] || fail "switch time '${bad}' must be refused, got ${wrapper_rc}"
  [[ ! -e "${dir}/calls" ]] || fail "switch time '${bad}' must be refused before touching the cluster"
  cases_run=$((cases_run + 1))
done

dir="$(new_wrapper_case catch-up-no-time)"
printf 'wedding-db-dedicated' >"${dir}/archive"
run_wrapper "${dir}" "${evaluator}" --confirm --catch-up
[[ "${wrapper_rc}" == 1 ]] || fail "a catch-up without a switch time must exit 1, got ${wrapper_rc}"
[[ ! -e "${dir}/calls" ]] || fail 'a catch-up without a switch time must not touch the cluster'
cases_run=$((cases_run + 1))

printf 'test-mirror-wedding-backup-catalogue: %d cases passed\n' "${cases_run}"
