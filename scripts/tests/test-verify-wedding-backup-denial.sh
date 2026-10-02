#!/usr/bin/env bash
# Exercise the Wedding backup denial proof without a cluster or credentials.
#
# The pod script runs against a fake mc that answers each access the way R2
# answers a bucket-scoped token, and the runner runs against a fake kubectl. The
# passing runner case consumes the real pod script's output, so the receipt the
# two scripts agree on cannot drift apart. With DENIAL_POD_RUNTIME_IMAGE set, the
# pod cases run inside the pinned production images instead of the host shell.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly runner="${root_dir}/scripts/verify-wedding-backup-denial.sh"
readonly pod_script="${root_dir}/scripts/verify-wedding-backup-denial-pod.sh"
work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT
readonly bin="${work_dir}/bin"
mkdir -p "${bin}"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

require_text() {
  if ! printf '%s' "$1" | grep -qF -- "$2"; then
    fail "$3: expected '$2'. Got: $1"
  fi
}

refute_text() {
  if printf '%s' "$1" | grep -qF -- "$2"; then
    fail "$3: did not expect '$2'. Got: $1"
  fi
}

readonly host='abc123.r2.cloudflarestorage.com'
readonly probe_id='4242-1'

# ---------------------------------------------------------------------------
# Fake mc. Every access by the dedicated alias to the shared bucket is answered
# from <case>/mc/<list|read|write>.out and .rc, which default to R2's refusal.
# Every failure also names the endpoint host, so redaction is exercised.
# ---------------------------------------------------------------------------
cat >"${bin}/mc" <<'FAKE'
#!/bin/sh
set -u
f="${FAKE_MC}"
printf '%s\n' "$*" >>"${f}/calls"
leak() {
  printf 'mc: <ERROR> request to https://abc123.r2.cloudflarestorage.com/x failed\n' >&2
}
# answer <name>: replay a scripted access result.
answer() {
  cat "${f}/$1.out"
  rc="$(cat "${f}/$1.rc")"
  if [ "${rc}" != 0 ]; then leak; fi
  exit "${rc}"
}
last=''
for arg in "$@"; do last="${arg}"; done
case "$1" in
  alias)
    printf '%s %s\n' "$3" "$5" >>"${f}/aliases"
    exit 0
    ;;
  ls)
    case "${last}" in
      dedicated/platform-backups/*) answer list ;;
      dedicated/*) answer own ;;
      shared/*) answer reference ;;
    esac
    ;;
  cp)
    case "$3" in
      dedicated/*)
        if [ "$(cat "${f}/read.rc")" = 0 ]; then printf 'shared backup bytes' >"$4"; fi
        answer read
        ;;
    esac
    case "$4" in
      dedicated/*) answer write ;;
    esac
    ;;
  rm)
    printf '%s\n' "${last}" >>"${f}/removed"
    exit 0
    ;;
esac
exit 64
FAKE
chmod +x "${bin}/mc"

# denied <operation>: R2's refusal of a bucket-scoped token, as mc --json prints it.
denied() {
  printf '{\n "status": "error",\n "error": {\n  "message": "Unable to %s.",\n  "cause": {\n   "message": "Access Denied.",\n   "error": {\n    "Code": "AccessDenied",\n    "Message": "Access Denied.",\n    "BucketName": "platform-backups",\n    "Server": "https://%s"\n   }\n  },\n  "type": "error"\n }\n}\n' "$1" "${host}"
}

# other_error <code>: a refusal for some reason other than authorisation.
other_error() {
  printf '{\n "status": "error",\n "error": {\n  "message": "Failed.",\n  "cause": {\n   "message": "Request to https://%s failed.",\n   "error": {\n    "Code": "%s"\n   }\n  },\n  "type": "error"\n }\n}\n' "${host}" "$1"
}

# file_record <key>: one object in an mc --json listing.
file_record() {
  printf '{"status":"success","type":"file","lastModified":"2026-09-01T00:00:00Z","size":512,"key":"%s","etag":"cccccccccccccccccccccccccccccccc","url":"https://%s","versionOrdinal":1,"storageClass":"STANDARD"}\n' "$1" "${host}"
}

# new_pod_case <name>: every refusal scripted as AccessDenied. Echoes the dir.
new_pod_case() {
  local dir="${work_dir}/pod-$1"
  mkdir -p "${dir}/mc" "${dir}/credentials/shared" "${dir}/credentials/dedicated" "${dir}/work"
  printf 'shared-id' >"${dir}/credentials/shared/ACCESS_KEY_ID"
  printf 'shared-secret' >"${dir}/credentials/shared/SECRET_ACCESS_KEY"
  printf 'dedicated-id' >"${dir}/credentials/dedicated/ACCESS_KEY_ID"
  printf 'dedicated-secret' >"${dir}/credentials/dedicated/SECRET_ACCESS_KEY"
  printf '{"status":"success","type":"folder","lastModified":"2026-09-09T00:00:00Z","size":0,"key":"wedding-db-20260909/","etag":"","url":"https://%s"}\n' "${host}" >"${dir}/mc/own.out"
  {
    printf '{"status":"success","type":"folder","lastModified":"2026-09-01T00:00:00Z","size":0,"key":"wedding-db/","etag":"","url":"https://%s"}\n' "${host}"
    file_record 'wedding-db/wals/0000000100000000/000000010000000000000042.gz'
    file_record 'wedding-db/wals/0000000100000000/000000010000000000000043.gz'
  } >"${dir}/mc/reference.out"
  printf '0' >"${dir}/mc/own.rc"
  printf '0' >"${dir}/mc/reference.rc"
  denied 'list folder' >"${dir}/mc/list.out"
  denied 'copy' >"${dir}/mc/read.out"
  denied 'upload' >"${dir}/mc/write.out"
  printf '1' >"${dir}/mc/list.rc"
  printf '1' >"${dir}/mc/read.rc"
  printf '1' >"${dir}/mc/write.rc"
  printf '%s' "${dir}"
}

# run_pod <dir>: runs the pod script; sets pod_rc, pod_out, pod_err, pod_calls.
run_pod() {
  local dir="$1"
  pod_rc=0
  if [[ -n "${DENIAL_POD_RUNTIME_IMAGE:-}" ]]; then
    # Synthetic fixtures only. Make the mounted fixtures writable by the real
    # non-root pod user; production obtains that access through fsGroup.
    chmod -R a+rwX "${dir}"
    docker run --rm --network none --read-only --user 65532:65532 \
      --cap-drop ALL --security-opt no-new-privileges \
      --entrypoint /tools/sh \
      -v "${bin}:/fake-bin:ro" -v "${dir}:${dir}" -v "${pod_script}:/denial/denial.sh:ro" \
      -e PATH=/fake-bin:/tools:/usr/local/bin:/usr/bin:/bin -e "FAKE_MC=${dir}/mc" \
      -e "CREDENTIALS_DIR=${dir}/credentials" -e "WORK_DIR=${dir}/work" \
      -e "ENDPOINT=${ENDPOINT_OVERRIDE:-https://${host}}" \
      -e "SHARED_BUCKET=${SHARED_OVERRIDE:-platform-backups}" -e SHARED_PREFIX=cnpg/wedding-db \
      -e DEDICATED_BUCKET=wedding-db-backups -e DEDICATED_PREFIX=cnpg/wedding-db \
      -e "PROBE_ID=${probe_id}" \
      "${DENIAL_POD_RUNTIME_IMAGE}" /denial/denial.sh >"${dir}/out" 2>"${dir}/err" || pod_rc=$?
  else
    PATH="${bin}:${PATH}" FAKE_MC="${dir}/mc" CREDENTIALS_DIR="${dir}/credentials" \
      WORK_DIR="${dir}/work" ENDPOINT="${ENDPOINT_OVERRIDE:-https://${host}}" \
      SHARED_BUCKET="${SHARED_OVERRIDE:-platform-backups}" SHARED_PREFIX=cnpg/wedding-db \
      DEDICATED_BUCKET=wedding-db-backups DEDICATED_PREFIX=cnpg/wedding-db \
      PROBE_ID="${probe_id}" \
      sh "${pod_script}" >"${dir}/out" 2>"${dir}/err" || pod_rc=$?
  fi
  pod_out="$(cat "${dir}/out")"
  pod_err="$(cat "${dir}/err")"
  pod_calls="$(cat "${dir}/mc/calls" 2>/dev/null || true)"
}

readonly receipt='{"dedicatedCatalogueReachable":true,"sharedCatalogueReferenced":true,"listDenied":true,"readDenied":true,"writeDenied":true}'
readonly marker='==== DENIAL OBSERVED ===='
readonly reference_path='platform-backups/cnpg/wedding-db/wedding-db/wals/0000000100000000/000000010000000000000042.gz'
readonly write_path="platform-backups/wedding-backup-denial-probe/${probe_id}"

# --- pod script --------------------------------------------------------------

# Every access refused with AccessDenied: the only passing outcome.
dir="$(new_pod_case denied)"
run_pod "${dir}"
[[ "${pod_rc}" -eq 0 ]] || fail "all three refusals must pass the proof (rc ${pod_rc}): ${pod_err}"
[[ "${pod_out}" == "${receipt}"$'\n'"${marker}" ]] || fail "the proof must print exactly the receipt and marker. Got: ${pod_out}"
require_text "$(cat "${dir}/mc/aliases")" 'shared shared-id' 'the shared alias uses the shared credential'
require_text "$(cat "${dir}/mc/aliases")" 'dedicated dedicated-id' 'the dedicated alias uses the dedicated credential'
require_text "${pod_calls}" 'ls --json dedicated/wedding-db-backups/cnpg/wedding-db/' 'the dedicated credential first lists its own catalogue'
require_text "${pod_calls}" 'ls --json --recursive shared/platform-backups/cnpg/wedding-db/' 'the shared credential names the refused object'
require_text "${pod_calls}" 'ls --json dedicated/platform-backups/cnpg/wedding-db/' 'the list is attempted with the dedicated credential'
require_text "${pod_calls}" "cp --json dedicated/${reference_path} " 'the read targets an object the shared listing named'
require_text "${pod_calls}" "dedicated/${write_path}" 'the write targets the run-owned probe key'
[[ ! -e "${dir}/mc/removed" ]] || fail 'nothing is removed when every access is refused'
[[ "$(grep -c '^ls --json dedicated/wedding-db-backups' <<<"${pod_calls}")" -eq 1 ]] || fail 'the own catalogue is listed once'

# One key in both Secrets cannot test the dedicated identity's scope.
dir="$(new_pod_case reuse)"
printf 'shared-id' >"${dir}/credentials/dedicated/ACCESS_KEY_ID"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'credential reuse must be refused'
require_text "${pod_err}" 'credential reuse' 'credential reuse names its reason'
refute_text "${pod_calls}" 'ls ' 'no access is attempted after a credential-reuse refusal'

# A dedicated credential that cannot reach its own catalogue makes every refusal meaningless.
dir="$(new_pod_case own-unreachable)"
printf '1' >"${dir}/mc/own.rc"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'an unreachable own catalogue must fail the proof'
require_text "${pod_err}" 'cannot list its own catalogue' 'the positive control names its reason'
refute_text "${pod_calls}" 'dedicated/platform-backups' 'no refusal is recorded before the positive control passes'
refute_text "${pod_err}${pod_out}" "${host}" 'the endpoint host is never printed'

dir="$(new_pod_case own-empty)"
: >"${dir}/mc/own.out"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'an empty own catalogue listing must fail the proof'
require_text "${pod_err}" 'empty or incomplete' 'an empty positive control names its reason'
refute_text "${pod_calls}" 'dedicated/platform-backups' 'no refusal is recorded after an empty positive control'

# A refusal of a target nobody showed exists proves nothing: a wrong bucket would pass.
dir="$(new_pod_case no-reference)"
grep -v '"type":"file"' "${dir}/mc/reference.out" >"${dir}/mc/reference.only-folders"
mv "${dir}/mc/reference.only-folders" "${dir}/mc/reference.out"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'a shared catalogue with no object must fail the proof'
require_text "${pod_err}" 'holds no object' 'a missing reference names its reason'
refute_text "${pod_calls}" 'dedicated/platform-backups' 'no refusal is recorded without a reference object'

dir="$(new_pod_case reference-unreadable)"
printf '1' >"${dir}/mc/reference.rc"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'an unreadable shared catalogue must fail the proof'
require_text "${pod_err}" 'cannot list the shared catalogue' 'an unreadable reference names its reason'
refute_text "${pod_err}${pod_out}" "${host}" 'the endpoint host is never printed'

dir="$(new_pod_case reference-partial)"
denied 'list folder' >>"${dir}/mc/reference.out"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'a shared listing with an error record must fail the proof'
require_text "${pod_err}" 'listing is incomplete' 'a partial reference listing names its reason'

# An accepted list is a broken isolation, and the other accesses are still reported.
dir="$(new_pod_case list-granted)"
file_record 'wedding-db/wals/0000000100000000/000000010000000000000042.gz' >"${dir}/mc/list.out"
printf '0' >"${dir}/mc/list.rc"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'an accepted list must fail the proof'
require_text "${pod_err}" 'ISOLATION BROKEN: the dedicated credential was allowed to list' 'an accepted list is reported as broken isolation'
require_text "${pod_calls}" "dedicated/${write_path}" 'the write is still attempted after an accepted list'
refute_text "${pod_out}" 'DENIAL OBSERVED' 'no receipt after an accepted list'

# An accepted list of an empty prefix prints no record at all, and is still accepted.
dir="$(new_pod_case list-empty-accepted)"
: >"${dir}/mc/list.out"
printf '0' >"${dir}/mc/list.rc"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'a list accepted with no output must fail the proof'
require_text "${pod_err}" 'allowed to list' 'a silent accepted list is reported as broken isolation'

# A success record wins even when the client also exits non-zero.
dir="$(new_pod_case list-partly-granted)"
{ file_record 'wedding-db/x'; denied 'list folder'; } >"${dir}/mc/list.out"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'a partly listed shared catalogue must fail the proof'
require_text "${pod_err}" 'allowed to list' 'a partly listed catalogue is reported as broken isolation'

dir="$(new_pod_case read-granted)"
printf '{"status":"success","source":"dedicated/%s","target":"/work/read-probe"}\n' "${reference_path}" >"${dir}/mc/read.out"
printf '0' >"${dir}/mc/read.rc"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'an accepted read must fail the proof'
require_text "${pod_err}" 'allowed to read' 'an accepted read is reported as broken isolation'
[[ ! -e "${dir}/work/read-probe" ]] || fail 'shared backup content read by the dedicated credential is removed'

# An accepted write is removed with the shared credential before the proof fails.
dir="$(new_pod_case write-granted)"
printf '{"status":"success","source":"/work/write-probe","target":"dedicated/%s"}\n' "${write_path}" >"${dir}/mc/write.out"
printf '0' >"${dir}/mc/write.rc"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'an accepted write must fail the proof'
require_text "${pod_err}" 'allowed to write' 'an accepted write is reported as broken isolation'
[[ "$(cat "${dir}/mc/removed" 2>/dev/null)" == "shared/${write_path}" ]] || fail 'an accepted write is removed through the shared credential'

# Any refusal other than AccessDenied is not evidence of denial.
dir="$(new_pod_case write-unproven)"
other_error NoSuchBucket >"${dir}/mc/write.out"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'a refusal other than AccessDenied must fail the proof'
require_text "${pod_err}" 'the write refusal is unproven: the client reported [ NoSuchBucket ]' 'an unproven refusal names the code the client reported'
refute_text "${pod_err}${pod_out}" "${host}" 'the endpoint host is never printed'
[[ "$(cat "${dir}/mc/removed" 2>/dev/null)" == "shared/${write_path}" ]] || fail 'an unproven write is cleaned up in case it landed'

dir="$(new_pod_case list-unproven)"
printf 'mc: <ERROR> Unable to list folder. dial tcp: lookup https://%s: i/o timeout\n' "${host}" >"${dir}/mc/list.out"
run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'a network failure must not pass as a refusal'
require_text "${pod_err}" 'the list refusal is unproven' 'a network failure is reported as unproven'
refute_text "${pod_err}${pod_out}" "${host}" 'the endpoint host is never printed'

dir="$(new_pod_case http)"
ENDPOINT_OVERRIDE="http://${host}" run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'a non-https endpoint must be refused'
[[ ! -e "${dir}/mc/calls" ]] || fail 'mc is never called with a non-https endpoint'

dir="$(new_pod_case same-bucket)"
SHARED_OVERRIDE=wedding-db-backups run_pod "${dir}"
[[ "${pod_rc}" -ne 0 ]] || fail 'one bucket on both sides must be refused'
require_text "${pod_err}" 'are the same bucket' 'one bucket on both sides names its reason'

# The passing pod output feeds the runner below.
dir="$(new_pod_case for-runner)"
run_pod "${dir}"
[[ "${pod_rc}" -eq 0 ]] || fail 'the runner fixture pod must pass'
cp "${dir}/out" "${work_dir}/passing-pod.log"

# --- runner ------------------------------------------------------------------

endpoint="$(sed -n 's/^  r2_endpoint: *//p' "${root_dir}/k8s/bases/bootstrap/config-map.yaml")"
shared_bucket="$(sed -n 's/^  r2_bucket: *//p' "${root_dir}/k8s/bases/bootstrap/config-map.yaml")"
readonly endpoint shared_bucket
[[ "${shared_bucket}" == platform-backups ]] || fail 'the committed shared bucket changed; review this proof'

cat >"${bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2 $3 $4 $5" == '--context admin@prod --namespace wedding-app --request-timeout=20s' ]] || exit 9
shift 5
printf '%s\n' "$*" >>"${CASE}/calls"
case "$1 $2" in
  'get clusters.postgresql.cnpg.io') cat "${CASE}/cluster.json" ;;
  'get objectstores.barmancloud.cnpg.io') cat "${CASE}/store-$3.json" ;;
  'create configmap') cp "${4#--from-file=denial.sh=}" "${CASE}/staged.sh" ;;
  'apply -f') cat >"${CASE}/manifest.yaml" ;;
  'get pod') cat "${CASE}/phase" ;;
  'logs pod/'*) cat "${CASE}/pod.log" ;;
  'delete pod' | 'delete configmap') printf '%s %s\n' "$2" "$3" >>"${CASE}/deleted" ;;
  *) exit 9 ;;
esac
FAKE
chmod +x "${bin}/kubectl"

# store <bucket> <secret> [endpoint]
store() {
  jq -n --arg path "s3://$1/cnpg/wedding-db" --arg secret "$2" --arg endpoint "${3:-${endpoint}}" '
    {apiVersion:"barmancloud.cnpg.io/v1",kind:"ObjectStore",metadata:{namespace:"wedding-app"},
     spec:{configuration:{destinationPath:$path,endpointURL:$endpoint,
       s3Credentials:{accessKeyId:{name:$secret,key:"ACCESS_KEY_ID"},
         secretAccessKey:{name:$secret,key:"SECRET_ACCESS_KEY"},
         region:{name:$secret,key:"REGION"}}}}}'
}

# new_case <name>: a cluster whose live state matches the reviewed proof.
new_case() {
  CASE="${work_dir}/runner-$1"
  export CASE
  mkdir -p "${CASE}"
  jq -n '{apiVersion:"postgresql.cnpg.io/v1",kind:"Cluster",metadata:{name:"wedding-db",namespace:"wedding-app"},
    spec:{plugins:[{name:"barman-cloud.cloudnative-pg.io",enabled:true,isWALArchiver:true,
      parameters:{barmanObjectName:"wedding-db-dedicated",serverName:"wedding-db-20260909"}}]}}' >"${CASE}/cluster.json"
  store platform-backups wedding-db-backup-r2 >"${CASE}/store-wedding-db.json"
  store wedding-db-backups wedding-db-backup-r2-dedicated >"${CASE}/store-wedding-db-dedicated.json"
  printf 'Succeeded' >"${CASE}/phase"
  cp "${work_dir}/passing-pod.log" "${CASE}/pod.log"
}

# run_runner [args...]: sets run_rc, run_out, run_err, run_calls.
run_runner() {
  run_rc=0
  PATH="${bin}:${PATH}" GITHUB_RUN_ID="${RUN_ID_OVERRIDE:-4242}" GITHUB_RUN_ATTEMPT=1 \
    DENIAL_POLL_INTERVAL=0 DENIAL_POLL_LIMIT=3 \
    bash "${runner}" "$@" >"${CASE}/out" 2>"${CASE}/err" || run_rc=$?
  run_out="$(cat "${CASE}/out")"
  run_err="$(cat "${CASE}/err")"
  run_calls="$(cat "${CASE}/calls" 2>/dev/null || true)"
}

new_case bare
run_runner
[[ "${run_rc}" -ne 0 ]] || fail 'a bare invocation must be refused'
require_text "${run_err}" 'Nothing has been touched' 'a bare invocation says nothing was touched'
[[ -z "${run_calls}" ]] || fail 'a bare invocation never calls kubectl'

new_case bad-run-id
RUN_ID_OVERRIDE='4242;rm' run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'an invalid run identifier must be refused'
[[ -z "${run_calls}" ]] || fail 'an invalid run identifier never calls kubectl'

new_case pass
run_runner --confirm
[[ "${run_rc}" -eq 0 ]] || fail "the passing proof must succeed (rc ${run_rc}): ${run_err}"
require_text "${run_out}" "${receipt}" 'the runner prints the receipt'
require_text "${run_out}" 'DENIAL OBSERVED' 'the runner names the verdict'
cmp -s "${CASE}/staged.sh" "${pod_script}" || fail 'the reviewed pod script is the one staged'
manifest="$(cat "${CASE}/manifest.yaml")"
require_text "${manifest}" 'secretName: wedding-db-backup-r2' 'the pod mounts the shared credential'
require_text "${manifest}" 'secretName: wedding-db-backup-r2-dedicated' 'the pod mounts the dedicated credential'
[[ "$(grep -c 'secretName:' <<<"${manifest}")" -eq 2 ]] || fail 'the pod mounts exactly two Secrets'
require_text "${manifest}" "value: \"${endpoint}\"" 'the pod uses the committed endpoint'
require_text "${manifest}" 'value: "platform-backups"' 'the pod targets the committed shared bucket'
require_text "${manifest}" 'value: "wedding-db-backups"' 'the pod lists the dedicated bucket'
require_text "${manifest}" 'value: "4242-1"' 'the probe key is owned by this run'
require_text "${manifest}" 'automountServiceAccountToken: false' 'the pod carries no service account token'
refute_text "${manifest}" '__' 'every manifest placeholder is replaced'
require_text "$(cat "${CASE}/deleted")" 'pod wedding-backup-denial-4242-1' 'the pod is removed'
require_text "$(cat "${CASE}/deleted")" 'configmap wedding-backup-denial-4242-1' 'the script ConfigMap is removed'

# The denial pod runs in the images the mirror runtime test exercises.
for image in mc_image tools_image; do
  [[ "$(grep "^readonly ${image}=" "${runner}")" == "$(grep "^readonly ${image}=" "${root_dir}/scripts/mirror-wedding-backup-catalogue.sh")" ]] ||
    fail "the denial proof and the catalogue mirror must pin the same ${image}"
done

new_case shared-archive
jq '.spec.plugins[0].parameters.barmanObjectName = "wedding-db"' "${CASE}/cluster.json" >"${CASE}/c" && mv "${CASE}/c" "${CASE}/cluster.json"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a Cluster archiving through the shared store must be refused'
require_text "${run_err}" 'does not archive through wedding-db-dedicated' 'an unused credential names its reason'
[[ ! -e "${CASE}/manifest.yaml" ]] || fail 'no pod starts when the credential is not the one in use'

new_case dedicated-drift
store other-bucket wedding-db-backup-r2-dedicated >"${CASE}/store-wedding-db-dedicated.json"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a drifted dedicated store must be refused'
require_text "${run_err}" 'wedding-db-dedicated ObjectStore is not wired' 'a drifted dedicated store names itself'
[[ ! -e "${CASE}/manifest.yaml" ]] || fail 'no pod starts for a drifted dedicated store'

new_case shared-drift
store other-bucket wedding-db-backup-r2 >"${CASE}/store-wedding-db.json"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a shared store off the committed bucket must be refused'
require_text "${run_err}" 'wedding-db ObjectStore is not wired' 'a drifted shared store names itself'

new_case endpoint-drift
store platform-backups wedding-db-backup-r2 'https://other.r2.cloudflarestorage.com' >"${CASE}/store-wedding-db.json"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a store at a foreign endpoint must be refused'
[[ ! -e "${CASE}/manifest.yaml" ]] || fail 'no credential is handed to a foreign endpoint'

new_case split-secret
jq '.spec.configuration.s3Credentials.secretAccessKey.name = "other"' "${CASE}/store-wedding-db.json" >"${CASE}/s" && mv "${CASE}/s" "${CASE}/store-wedding-db.json"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a credential split across Secrets must be refused'

new_case same-bucket-config
sed 's/^  r2_bucket: .*/  r2_bucket: wedding-db-backups/' "${root_dir}/k8s/bases/bootstrap/config-map.yaml" >"${CASE}/config-map.yaml"
DENIAL_BOOTSTRAP_CONFIG="${CASE}/config-map.yaml" run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a committed shared bucket equal to the dedicated one must be refused'
[[ -z "${run_calls}" ]] || fail 'a broken committed configuration never calls kubectl'

new_case pod-failed
printf 'Failed' >"${CASE}/phase"
printf 'denial-pod: ISOLATION BROKEN: the dedicated credential was allowed to write the shared destination\ndenial-pod: the shared destination did not refuse every access with AccessDenied\n' >"${CASE}/pod.log"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a failed pod must fail the proof'
require_text "${run_err}" 'ISOLATION BROKEN' 'a failed pod surfaces its reason'
require_text "$(cat "${CASE}/deleted")" 'pod wedding-backup-denial-4242-1' 'a failed pod is removed'

new_case partial-receipt
sed 's/"writeDenied":true/"writeDenied":false/' "${work_dir}/passing-pod.log" >"${CASE}/pod.log"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a receipt that is not complete must fail the proof'
require_text "${run_err}" 'without its complete receipt' 'an incomplete receipt names its reason'

new_case reordered
{ printf '%s\n' "${marker}"; printf '%s\n' "${receipt}"; } >"${CASE}/pod.log"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a marker printed before the receipt must fail the proof'

new_case trailing
{ cat "${work_dir}/passing-pod.log"; printf 'denial-pod: late\n'; } >"${CASE}/pod.log"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'output after the marker must fail the proof'

new_case never-finishes
printf 'Running' >"${CASE}/phase"
run_runner --confirm
[[ "${run_rc}" -ne 0 ]] || fail 'a pod that never finishes must fail the proof'
require_text "${run_err}" "phase 'Running'" 'a pod that never finishes names its phase'
require_text "$(cat "${CASE}/deleted")" 'configmap wedding-backup-denial-4242-1' 'a pod that never finishes is cleaned up'
[[ "$(grep -c '^get pod ' "${CASE}/calls")" -eq 3 ]] || fail 'the phase poll is bounded'

# --- workflow ----------------------------------------------------------------

# The job holds the production kubeconfig and both backup credentials reach the
# pod it starts, so only an operator on main may start it, under the deploy lock.
workflow="${root_dir}/.github/workflows/verify-wedding-backup-denial.yaml"
readonly workflow
[[ "$(yq -r '.on | keys | join(",")' "${workflow}")" == workflow_dispatch ]] ||
  fail 'the proof must be triggered by workflow_dispatch only'
[[ "$(yq -r '.permissions | length' "${workflow}")" == 0 ]] || fail 'the workflow grants no default permissions'
[[ "$(yq -r '.jobs | keys | join(",")' "${workflow}")" == denial ]] || fail 'the workflow runs exactly the denial job'
[[ "$(yq -r '.jobs.denial.permissions | to_entries | map(.key + "=" + .value) | join(",")' "${workflow}")" == contents=read ]] ||
  fail 'the denial job may only read the repository'
[[ "$(yq -r '.jobs.denial.environment' "${workflow}")" == prod ]] || fail 'the denial job runs in the prod environment'
[[ "$(yq -r '.concurrency.group + " " + (.concurrency."cancel-in-progress" | tostring) + " " + .concurrency.queue' "${workflow}")" == 'prod-deploy false max' ]] ||
  fail 'the proof serializes with production deploys and is never cancelled by one'
guard="$(yq -r '.jobs.denial.steps[0].run' "${workflow}")"
require_text "${guard}" "!= 'refs/heads/main'" 'the first step refuses any ref but main'
require_text "${guard}" "!= 'verify-wedding-backup-denial'" 'the first step requires the exact confirmation'
# shellcheck disable=SC2016 # the literal GitHub expression opener is what is refused
refute_text "$(yq -r '.jobs.denial.steps[].run // ""' "${workflow}")" '${{' 'no expression is expanded inside a run script'
[[ "$(yq -r '.jobs.denial.steps[-1].run' "${workflow}")" == './scripts/verify-wedding-backup-denial.sh --confirm' ]] ||
  fail 'the workflow runs the reviewed proof'

printf 'PASS: Wedding backup denial proof\n'
