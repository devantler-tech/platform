#!/usr/bin/env bash

# Pin the behaviour of the shared Wedding backup coverage proof:
# scripts/verify-wedding-shared-backup-coverage.sh (runs on the runner) and
# scripts/verify-wedding-shared-backup-coverage-pod.sh (runs in the cluster).
#
# WHY THIS EXISTS. The stale shared copy of the Wedding catalogue is removed on
# this proof's word (#4481), and that removal cannot be undone. Its dangerous
# mistakes are quiet ones:
#
#   * reporting COVERED when the dedicated bucket lacks an object, holds different
#     bytes, or was listed before the shared one;
#   * reporting COVERED while something can still write to the shared copy;
#   * handing a credential to an endpoint or bucket nobody reviewed;
#   * writing to or deleting from either bucket.
#
# Every case runs the REAL runner, the REAL pod script and the REAL evaluator
# against a fake kubectl and a fake mc, so the three are proven to agree on the
# listing and checksum formats. Needs Go and jq, no cluster and no credentials.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly runner="${root_dir}/scripts/verify-wedding-shared-backup-coverage.sh"
readonly pod_script="${root_dir}/scripts/verify-wedding-shared-backup-coverage-pod.sh"
readonly mirror="${root_dir}/scripts/mirror-wedding-backup-catalogue.sh"

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
# Fake mc. Only `alias set`, `ls` and `cat` are served. Anything else is recorded
# as forbidden, and every case asserts nothing forbidden was attempted. Every
# failure writes the endpoint host to stderr, so redaction is exercised.
# ---------------------------------------------------------------------------
cat >"${bin}/mc" <<'FAKE'
#!/bin/sh
set -u
f="${FAKE_MC}"
leak() {
  printf 'mc: <ERROR> request to https://abc123.r2.cloudflarestorage.com/x failed\n' >&2
}
case "$1" in
  alias)
    if [ "$2" != set ]; then printf '%s\n' "$*" >>"${f}/forbidden"; exit 64; fi
    printf '%s %s\n' "$3" "$5" >>"${f}/aliases"
    exit 0
    ;;
  ls)
    printf '%s\n' "$4" >>"${f}/listed"
    if [ -e "${f}/ls-rc" ]; then leak; exit "$(cat "${f}/ls-rc")"; fi
    cat "${f}/ls"
    exit 0
    ;;
  cat)
    printf '%s\n' "$2" >>"${f}/catted"
    if [ -e "${f}/cat-rc" ]; then leak; exit "$(cat "${f}/cat-rc")"; fi
    if [ -e "${f}/content" ]; then cat "${f}/content"; else printf 'bytes of %s' "${2##*/}"; fi
    exit 0
    ;;
esac
printf '%s\n' "$*" >>"${f}/forbidden"
exit 64
FAKE
chmod +x "${bin}/mc"

# ---------------------------------------------------------------------------
# Fake kubectl. `exec` runs the real pod script with the environment the runner
# wrote into that namespace's pod manifest, so the manifest wiring is part of
# what each case proves.
# ---------------------------------------------------------------------------
cat >"${bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -u
k="${FAKE_K}"
namespace=''
args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) shift 2 ;;
    --namespace) namespace="$2"; shift 2 ;;
    --request-timeout=*) shift ;;
    *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]}"
printf '%s %s\n' "${namespace:-all}" "$*" >>"${k}/calls"

manifest_env() {
  awk -v want="$2" '$0 ~ "- name: " want "$" { getline; gsub(/^ *value: *"?|"?$/, ""); print; exit }' "${k}/pod-$1.yaml"
}

case "$1" in
  get)
    case "$2" in
      pod)
        cat "${k}/phase-${namespace}" 2>/dev/null || printf 'Running'
        ;;
      clusters.postgresql.cnpg.io | objectstores.barmancloud.cnpg.io | externalsecrets.external-secrets.io | secrets)
        if [ "$3" = --all-namespaces ]; then
          cat "${k}/all-$2.json" || exit 1
        elif [ "${4:-}" = --ignore-not-found ]; then
          if [ -e "${k}/absence-rc" ]; then exit "$(cat "${k}/absence-rc")"; fi
          if [ -e "${k}/present-$2-$3" ]; then printf '%s/%s\n' "$2" "$3"; fi
        else
          cat "${k}/${namespace}-$2-$3.json" 2>/dev/null || exit 1
        fi
        ;;
      *) exit 64 ;;
    esac
    ;;
  create)
    if [ "$2" = configmap ]; then
      exit 0
    fi
    cat >"${k}/pod-${namespace}.yaml"
    ;;
  logs)
    if [ ! -e "${k}/never-ready-${namespace}" ]; then printf '==== COVERAGE POD READY ====\n'; fi
    ;;
  delete)
    printf '%s %s\n' "${namespace}" "$2" >>"${k}/deleted"
    ;;
  exec)
    while [ "$1" != -- ]; do shift; done
    shift
    if [ "$1" = cat ]; then
      cat "${k}/work-${namespace}/${2#/work/}"
      exit $?
    fi
    [ "$1 $2" = '/tools/sh /coverage/coverage.sh' ] || exit 64
    PATH="${FAKE_BIN}:${PATH}" FAKE_MC="${k}/mc-${namespace}" \
      ENDPOINT="$(manifest_env "${namespace}" ENDPOINT)" ROLE="$(manifest_env "${namespace}" ROLE)" \
      BUCKET="$(manifest_env "${namespace}" BUCKET)" PREFIX="$(manifest_env "${namespace}" PREFIX)" \
      WORK_DIR="${k}/work-${namespace}" CREDENTIALS_DIR="${k}/credentials" \
      sh "${POD_SCRIPT}" "$3"
    ;;
  *) exit 64 ;;
esac
FAKE
chmod +x "${bin}/kubectl"

readonly endpoint='https://abc123.r2.cloudflarestorage.com'
readonly old='2026-09-01T00:00:00Z'
readonly base='wedding-db-20260909/base/20260908T030000'
readonly wal='wedding-db-20260909/wals/0000000200000001/000000020000000100000003.gz'
readonly later_wal='wedding-db-20260909/wals/0000000300000001/000000030000000100000009.gz'

# record <key> <size> <etag>
record() {
  printf '{"status":"success","type":"file","lastModified":"%s","size":%s,"key":"%s","etag":"%s","url":"%s","versionOrdinal":1,"storageClass":"STANDARD"}\n' \
    "${old}" "$2" "$1" "$3" "${endpoint}"
}

# store_json <destination> <secret> [endpoint]
store_json() {
  printf '{"metadata":{},"spec":{"configuration":{"destinationPath":"%s","endpointURL":"%s","s3Credentials":{"accessKeyId":{"name":"%s","key":"ACCESS_KEY_ID"},"secretAccessKey":{"name":"%s","key":"SECRET_ACCESS_KEY"}}}}}\n' \
    "$1" "${3:-${endpoint}}" "$2" "$2"
}

# cluster_json <archive store>
cluster_json() {
  printf '{"metadata":{"uid":"cluster-uid-1","generation":3},"spec":{"instances":1,"plugins":[{"name":"barman-cloud.cloudnative-pg.io","enabled":true,"isWALArchiver":true,"parameters":{"barmanObjectName":"%s"}}]},"status":{"readyInstances":1,"conditions":[{"type":"Ready","status":"True"},{"type":"ContinuousArchiving","status":"True"}]}}\n' "$1"
}

# new_case prepares a covered scenario; each case then breaks one thing.
new_case() {
  cases_run=$((cases_run + 1))
  k="${work_dir}/case-${cases_run}"
  mkdir -p "${k}/mc-umami" "${k}/mc-wedding-app" "${k}/work-umami" "${k}/work-wedding-app" "${k}/credentials"
  printf 'data:\n  r2_endpoint: %s\n  r2_bucket: platform-backups\n' "${endpoint}" >"${k}/config-map.yaml"
  printf 'AKIDEXAMPLE' >"${k}/credentials/ACCESS_KEY_ID"
  printf 'secret-value' >"${k}/credentials/SECRET_ACCESS_KEY"
  cluster_json wedding-db-dedicated >"${k}/wedding-app-clusters.postgresql.cnpg.io-wedding-db.json"
  store_json s3://wedding-db-backups/cnpg/wedding-db wedding-db-backup-r2-dedicated \
    >"${k}/wedding-app-objectstores.barmancloud.cnpg.io-wedding-db-dedicated.json"
  store_json s3://platform-backups/cnpg/umami-db umami-db-backup-r2 \
    >"${k}/umami-objectstores.barmancloud.cnpg.io-umami-db.json"
  printf '{"items":[{"spec":{"configuration":{"destinationPath":"s3://platform-backups/cnpg/umami-db"}}},{"spec":{"configuration":{"destinationPath":"s3://wedding-db-backups/cnpg/wedding-db"}}}]}\n' \
    >"${k}/all-objectstores.barmancloud.cnpg.io.json"
  printf '{"items":[{"spec":{"instances":1}}]}\n' >"${k}/all-clusters.postgresql.cnpg.io.json"
  {
    record "${base}/backup.info" 1200 a1
    record "${base}/data.tar.gz" 90000000 b2-6
    record "${wal}" 4000 c3
  } >"${k}/mc-umami/ls"
  # The mirror uploaded the archive in a different number of parts, and the
  # dedicated store has archived more since the switch.
  {
    record "${base}/backup.info" 1200 a1
    record "${base}/data.tar.gz" 90000000 ff-4
    record "${wal}" 4000 c3
    record "${later_wal}" 4300 f6
  } >"${k}/mc-wedding-app/ls"
}

# run_case [runner arguments...] sets out, err and rc.
run_case() {
  rc=0
  FAKE_K="${k}" FAKE_BIN="${bin}" POD_SCRIPT="${pod_script}" KUBECTL="${bin}/kubectl" \
    COVERAGE_EVALUATOR="${evaluator}" COVERAGE_BOOTSTRAP_CONFIG="${k}/config-map.yaml" \
    COVERAGE_POLL_INTERVAL=0 COVERAGE_POLL_LIMIT=2 GITHUB_RUN_ID=77 GITHUB_RUN_ATTEMPT=1 \
    bash "${runner}" "$@" >"${k}/out" 2>"${k}/err" || rc=$?
  out="$(cat "${k}/out")"
  err="$(cat "${k}/err")"
  for side in umami wedding-app; do
    [[ ! -e "${k}/mc-${side}/forbidden" ]] ||
      fail "case ${cases_run}: the pod ran an mc command other than alias set, ls or cat: $(cat "${k}/mc-${side}/forbidden")"
  done
}

# require_untouched asserts a refusal happened before anything was created.
require_untouched() {
  [[ "${rc}" -eq 1 ]] || fail "$1: expected exit 1, got ${rc}. stderr: ${err}"
  if grep -q ' create ' "${k}/calls" 2>/dev/null; then
    fail "$1: something was created before the refusal"
  fi
  refute_text "${out}" COVERED "$1"
}

# require_cleaned asserts both pods and both scripts were removed.
require_cleaned() {
  for entry in 'umami pod' 'umami configmap' 'wedding-app pod' 'wedding-app configmap'; do
    grep -qxF "${entry}" "${k}/deleted" || fail "$1: ${entry} was not removed"
  done
}

# ---------------------------------------------------------------------------
# Covered.
# ---------------------------------------------------------------------------
new_case
run_case --confirm
[[ "${rc}" -eq 0 ]] || fail "covered: expected exit 0, got ${rc}. stderr: ${err}"
require_text "${out}" 'COVERED: the dedicated bucket holds every object of the shared Wedding catalogue (3 objects)' 'covered'
require_text "${out}" '"sharedObjects":3,"matchedObjects":3,"retentionPrunedObjects":0,"covered":true' 'covered'
require_cleaned 'covered'
# One credential per pod, each beside its own namespace's Secret.
require_text "$(cat "${k}/pod-umami.yaml")" 'secretName: umami-db-backup-r2' 'shared pod credential'
refute_text "$(cat "${k}/pod-umami.yaml")" 'wedding-db-backup-r2' 'shared pod credential'
require_text "$(cat "${k}/pod-wedding-app.yaml")" 'secretName: wedding-db-backup-r2-dedicated' 'dedicated pod credential'
refute_text "$(cat "${k}/pod-wedding-app.yaml")" 'umami-db-backup-r2' 'dedicated pod credential'
[[ "$(grep -c 'secretName:' "${k}/pod-umami.yaml")" -eq 1 && "$(grep -c 'secretName:' "${k}/pod-wedding-app.yaml")" -eq 1 ]] ||
  fail 'a pod mounts more than one credential'
# Each pod lists only the Wedding catalogue of its own bucket.
[[ "$(cat "${k}/mc-umami/listed")" == 'store/platform-backups/cnpg/wedding-db/' ]] || fail 'the shared pod listed something else'
[[ "$(cat "${k}/mc-wedding-app/listed")" == 'store/wedding-db-backups/cnpg/wedding-db/' ]] || fail 'the dedicated pod listed something else'
# Only the object an ETag cannot prove is read, on both sides.
[[ "$(cat "${k}/mc-umami/catted")" == "store/platform-backups/cnpg/wedding-db/${base}/data.tar.gz" ]] || fail 'the shared pod hashed the wrong objects'
[[ "$(cat "${k}/mc-wedding-app/catted")" == "store/wedding-db-backups/cnpg/wedding-db/${base}/data.tar.gz" ]] || fail 'the dedicated pod hashed the wrong objects'
refute_text "${out}${err}" 'abc123' 'covered: endpoint host leaked'
printf 'PASS: a covered catalogue is reported as covered and both pods are removed\n'

# Retention runs on the dedicated store only, so the shared copy outlives backups
# the dedicated store has pruned.
new_case
{
  record 'wedding-db-20260909/base/20260801T030000/backup.info' 1100 p1
  record 'wedding-db-20260909/base/20260801T030000/data.tar.gz' 80000000 p2-5
  record 'wedding-db-20260909/wals/0000000200000000/0000000200000000000000FE.gz' 3900 p3
} >>"${k}/mc-umami/ls"
run_case --confirm
[[ "${rc}" -eq 0 ]] || fail "retention: expected exit 0, got ${rc}. stderr: ${err}"
require_text "${out}" '"sharedObjects":6,"matchedObjects":3,"retentionPrunedObjects":3,"covered":true' 'retention'
printf 'PASS: objects the dedicated store pruned by retention do not block the verdict\n'

# After the shared copy is removed, the same proof reports that.
new_case
: >"${k}/mc-umami/ls"
run_case --confirm
[[ "${rc}" -eq 0 ]] || fail "empty: expected exit 0, got ${rc}. stderr: ${err}"
require_text "${out}" 'EMPTY: the shared bucket holds no Wedding catalogue' 'empty'
refute_text "${out}" 'COVERED' 'empty'
require_cleaned 'empty'
printf 'PASS: an empty shared prefix is reported as empty, not as covered\n'

# ---------------------------------------------------------------------------
# Not covered.
# ---------------------------------------------------------------------------
new_case
record "${later_wal}" 4300 f6 >>"${k}/mc-umami/ls"
{
  record "${base}/backup.info" 1200 a1
  record "${base}/data.tar.gz" 90000000 ff-4
  record "${wal}" 4000 c3
} >"${k}/mc-wedding-app/ls"
run_case --confirm
[[ "${rc}" -eq 1 ]] || fail "missing object: expected exit 1, got ${rc}"
require_text "${err}" 'NOT COVERED' 'missing object'
refute_text "${out}" 'COVERED:' 'missing object'
require_cleaned 'missing object'
printf 'PASS: a shared object the dedicated bucket lacks is not covered\n'

new_case
printf 'different bytes' >"${k}/mc-wedding-app/content"
run_case --confirm
[[ "${rc}" -eq 1 ]] || fail "different bytes: expected exit 1, got ${rc}"
require_text "${err}" 'checksum mismatch' 'different bytes'
require_text "${err}" 'NOT COVERED' 'different bytes'
printf 'PASS: a multipart object with different bytes is not covered\n'

new_case
{
  record "${base}/backup.info" 1200 zz
  record "${base}/data.tar.gz" 90000000 ff-4
  record "${wal}" 4000 c3
} >"${k}/mc-wedding-app/ls"
run_case --confirm
[[ "${rc}" -eq 1 ]] || fail "different etag: expected exit 1, got ${rc}"
require_text "${err}" 'NOT COVERED' 'different etag'
printf 'PASS: a single-part object with a different ETag is not covered\n'

new_case
printf '1' >"${k}/mc-wedding-app/cat-rc"
run_case --confirm
[[ "${rc}" -eq 1 ]] || fail "unreadable object: expected exit 1, got ${rc}"
require_text "${err}" 'the dedicated pod could not hash its objects' 'unreadable object'
refute_text "${out}${err}" 'abc123' 'unreadable object: endpoint host leaked'
require_text "${err}" '<endpoint>' 'unreadable object'
printf 'PASS: an object that cannot be read is not covered, and the endpoint host is redacted\n'

new_case
printf '1' >"${k}/mc-umami/ls-rc"
run_case --confirm
[[ "${rc}" -eq 1 ]] || fail "failed listing: expected exit 1, got ${rc}"
require_text "${err}" 'the shared pod could not list the shared catalogue' 'failed listing'
refute_text "${out}" 'EMPTY' 'failed listing'
require_cleaned 'failed listing'
printf 'PASS: a failed listing is not an empty catalogue\n'

new_case
printf '{"status":"error","error":{"message":"denied"}}\n' >>"${k}/mc-umami/ls"
run_case --confirm
[[ "${rc}" -eq 1 ]] || fail "error record: expected exit 1, got ${rc}"
require_text "${err}" 'returned a record that is not a success' 'error record'
printf 'PASS: a listing carrying an error record is refused\n'

new_case
touch "${k}/never-ready-umami"
run_case --confirm
[[ "${rc}" -eq 1 ]] || fail "never ready: expected exit 1, got ${rc}"
require_text "${err}" 'the shared pod never became ready' 'never ready'
require_cleaned 'never ready'
printf 'PASS: a pod that never becomes ready fails the proof and is removed\n'

# ---------------------------------------------------------------------------
# Refusals before anything is created.
# ---------------------------------------------------------------------------
new_case
run_case
require_untouched 'no confirmation'
require_text "${err}" 'refusing to run: use --confirm' 'no confirmation'
[[ ! -e "${k}/calls" ]] || fail 'no confirmation: the cluster was contacted'

new_case
run_case --confirm --delete
require_untouched 'extra argument'

new_case
cluster_json wedding-db >"${k}/wedding-app-clusters.postgresql.cnpg.io-wedding-db.json"
run_case --confirm
require_untouched 'still archiving to the shared store'
require_text "${err}" 'does not archive through wedding-db-dedicated' 'still archiving to the shared store'

for present in objectstores.barmancloud.cnpg.io-wedding-db externalsecrets.external-secrets.io-wedding-db-backup-r2 secrets-wedding-db-backup-r2; do
  new_case
  touch "${k}/present-${present}"
  run_case --confirm
  require_untouched "wedding-app still holds ${present}"
  require_text "${err}" 'shared backup access has not been retired' "wedding-app still holds ${present}"
done

# A failed read is not an absence.
new_case
printf '1' >"${k}/absence-rc"
run_case --confirm
require_untouched 'absence check failed'
require_text "${err}" 'could not check whether wedding-app still holds' 'absence check failed'

new_case
printf '{"items":[{"spec":{"configuration":{"destinationPath":"s3://platform-backups/cnpg/wedding-db"}}}]}\n' \
  >"${k}/all-objectstores.barmancloud.cnpg.io.json"
run_case --confirm
require_untouched 'a store still names the shared catalogue'
require_text "${err}" 'still names the shared Wedding catalogue' 'a store still names the shared catalogue'

new_case
printf '{"items":[{"spec":{"externalClusters":[{"barmanObjectStore":{"destinationPath":"s3://platform-backups/cnpg/wedding-db/"}}]}}]}\n' \
  >"${k}/all-clusters.postgresql.cnpg.io.json"
run_case --confirm
require_untouched 'a Cluster still names the shared catalogue'

# A sibling catalogue whose name merely starts the same way is not a reference.
new_case
printf '{"items":[{"spec":{"configuration":{"destinationPath":"s3://platform-backups/cnpg/wedding-db-archive"}}}]}\n' \
  >"${k}/all-objectstores.barmancloud.cnpg.io.json"
run_case --confirm
[[ "${rc}" -eq 0 ]] || fail "sibling catalogue: expected exit 0, got ${rc}. stderr: ${err}"

new_case
rm "${k}/all-clusters.postgresql.cnpg.io.json"
run_case --confirm
require_untouched 'reference listing failed'
require_text "${err}" 'could not list every clusters.postgresql.cnpg.io' 'reference listing failed'

new_case
store_json s3://wedding-db-backups/cnpg/other wedding-db-backup-r2-dedicated \
  >"${k}/wedding-app-objectstores.barmancloud.cnpg.io-wedding-db-dedicated.json"
run_case --confirm
require_untouched 'dedicated store drifted'

new_case
store_json s3://other-bucket/cnpg/umami-db umami-db-backup-r2 >"${k}/umami-objectstores.barmancloud.cnpg.io-umami-db.json"
run_case --confirm
require_untouched 'shared reference store names another bucket'

new_case
store_json s3://platform-backups/cnpg/umami-db other-secret >"${k}/umami-objectstores.barmancloud.cnpg.io-umami-db.json"
run_case --confirm
require_untouched 'shared reference store names another Secret'

new_case
store_json s3://platform-backups/cnpg/umami-db umami-db-backup-r2 https://evil.example.com \
  >"${k}/umami-objectstores.barmancloud.cnpg.io-umami-db.json"
run_case --confirm
require_untouched 'shared reference store names another endpoint'

new_case
printf 'data:\n  r2_endpoint: %s\n  r2_bucket: wedding-db-backups\n' "${endpoint}" >"${k}/config-map.yaml"
run_case --confirm
require_untouched 'committed shared bucket is the dedicated bucket'

new_case
printf 'data:\n  r2_endpoint: http://plain.example.com\n  r2_bucket: platform-backups\n' >"${k}/config-map.yaml"
run_case --confirm
require_untouched 'committed endpoint is not https'
printf 'PASS: every refusal happens before a pod or script is created\n'

# ---------------------------------------------------------------------------
# The pod script on its own.
# ---------------------------------------------------------------------------
# pod <role> <bucket> <prefix> <command> sets pod_out and pod_rc.
pod() {
  pod_rc=0
  pod_out="$(PATH="${bin}:${PATH}" FAKE_MC="${k}/mc-umami" ENDPOINT="${POD_ENDPOINT:-${endpoint}}" \
    ROLE="$1" BUCKET="$2" PREFIX="$3" WORK_DIR="${k}/work-umami" CREDENTIALS_DIR="${k}/credentials" \
    SERVE_TIMEOUT=0 sh "${pod_script}" "$4" 2>&1 </dev/null)" || pod_rc=$?
}

new_case
pod shared platform-backups cnpg/wedding-db serve
[[ "${pod_rc}" -eq 0 ]] || fail "serve: expected exit 0, got ${pod_rc}: ${pod_out}"
require_text "${pod_out}" '==== COVERAGE POD READY ====' 'serve'
[[ "$(cat "${k}/mc-umami/aliases")" == "store AKIDEXAMPLE" ]] || fail 'serve did not store the credential under one alias'
refute_text "${pod_out}" 'secret-value' 'serve: credential leaked'

pod shared platform-backups cnpg wedding
[[ "${pod_rc}" -eq 1 ]] || fail 'a prefix other than the reviewed one was accepted'
require_text "${pod_out}" 'the catalogue prefix is not the reviewed one' 'wrong prefix'
pod shared wedding-db-backups cnpg/wedding-db list
[[ "${pod_rc}" -eq 1 ]] || fail 'the shared side accepted the dedicated bucket'
pod dedicated platform-backups cnpg/wedding-db list
[[ "${pod_rc}" -eq 1 ]] || fail 'the dedicated side accepted another bucket'
pod other platform-backups cnpg/wedding-db list
[[ "${pod_rc}" -eq 1 ]] || fail 'an unknown role was accepted'
POD_ENDPOINT='http://plain.example.com' pod shared platform-backups cnpg/wedding-db list
[[ "${pod_rc}" -eq 1 ]] || fail 'a plain http endpoint was accepted'
for unknown in remove delete rm mirror ''; do
  pod shared platform-backups cnpg/wedding-db "${unknown}"
  [[ "${pod_rc}" -eq 1 ]] || fail "the pod accepted the command '${unknown}'"
  require_text "${pod_out}" 'unknown command' "command '${unknown}'"
done
pod shared platform-backups cnpg/wedding-db hash
[[ "${pod_rc}" -eq 1 ]] || fail 'hash ran without a listing'
[[ ! -e "${k}/mc-umami/forbidden" ]] || fail 'the pod script ran a forbidden mc command'
cases_run=$((cases_run + 1))
printf 'PASS: the pod script refuses unreviewed locations and has no command that writes\n'

# The pod script may only ever read. Every mc invocation in it must be one of
# the three read-side commands, so a later edit cannot add a write quietly.
invocations="$(grep -v -e '^ *#' -e '^for tool in ' "${pod_script}" | grep -oE '(^|[^A-Za-z_"])mc [a-z]+( set)?' | sed -E 's/^[^m]*//' | sort -u)"
[[ "${invocations}" == $'mc alias set\nmc cat\nmc ls' ]] ||
  fail "the pod script runs mc commands other than alias set, cat and ls: ${invocations}"
printf 'PASS: the pod script only runs mc alias set, mc ls and mc cat\n'

# The pinned images are the ones the mirror runtime test exercises.
for image in mc_image tools_image; do
  ours="$(sed -n "s/^readonly ${image}='\\([^']*\\)'\$/\\1/p" "${runner}")"
  theirs="$(sed -n "s/^readonly ${image}='\\([^']*\\)'\$/\\1/p" "${mirror}")"
  [[ -n "${ours}" && "${ours}" == "${theirs}" ]] || fail "${image} differs from the mirror's pinned image"
done
printf 'PASS: the proof pins the same images as the catalogue mirror\n'

printf '\nAll %s cases passed.\n' "${cases_run}"
