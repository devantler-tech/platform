#!/usr/bin/env bash

# Mirror the shared Wedding backup catalogue into its dedicated bucket and prove
# the copy with the reviewed evaluator (scripts/mirror-wedding-backup-catalogue).
#
# This is the copy step #3252's cutover waits on: the Cluster may only switch its
# archive reference to wedding-db-dedicated once the dedicated bucket holds the
# full recoverable history.
#
# WHAT IT DOES, in order, refusing at the first thing it cannot prove:
#   1. Confirms the live Cluster still archives through `wedding-db`.
#   2. Reads both live ObjectStores and hands their exact bucket, prefix and
#      Secret names to `validate-plan`, so a drifted store is refused rather than
#      replaced with the reviewed value.
#   3. Runs scripts/mirror-wedding-backup-catalogue-pod.sh in a short-lived pod in
#      wedding-app, where both credentials and R2 egress already exist.
#   4. Confirms the Cluster STILL archives through `wedding-db`.
#   5. Runs `evaluate` on the three listings the pod emitted.
#
# Exit status: 0 converged (the destination is ready for the cutover), 3 copied
# and verified but not converged (objects archived during the run are still
# pending, so run it again), 1 refused or failed. Only 0 is cutover proof.
#
# Needs --confirm. The pod writes to a production bucket, so a bare invocation
# does nothing.

set -euo pipefail

readonly context='admin@prod'
readonly namespace='wedding-app'
readonly cluster='wedding-db'
readonly source_store='wedding-db'
readonly destination_store='wedding-db-dedicated'
readonly plugin='barman-cloud.cloudnative-pg.io'
# Same digest-pinned client the DR rebuild uses to read R2.
readonly mc_image='quay.io/minio/mc:RELEASE.2025-04-08T15-39-49Z@sha256:7e3efb09c22c0882fbf341b9d99f61f94ae6c4c20a06f2f1a2b20ea8993d8952'

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly pod_script="${root_dir}/scripts/mirror-wedding-backup-catalogue-pod.sh"

kubectl_bin="${KUBECTL:-kubectl}"
readonly kubectl_bin
poll_interval="${MIRROR_POLL_INTERVAL:-10}"
poll_limit="${MIRROR_POLL_LIMIT:-1080}"
readonly poll_interval poll_limit

work_dir="$(mktemp -d)"
readonly work_dir
created=false
name=''
# Invoked by the EXIT trap below. shellcheck reports this trap-only handler as
# unused (SC2329 on 0.11) or unreachable (SC2317 on the older CI runner); the
# test suite asserts both deletions actually happen.
# shellcheck disable=SC2317,SC2329
cleanup() {
  if [[ "${created}" == true ]]; then
    kube delete pod "${name}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kube delete configmap "${name}" --ignore-not-found >/dev/null 2>&1 || true
  fi
  rm -rf "${work_dir}"
}
trap cleanup EXIT

fail() {
  printf 'mirror-wedding-backup-catalogue: %s\n' "$1" >&2
  exit 1
}

if [[ "$#" -ne 1 || "$1" != '--confirm' ]]; then
  fail 'refusing to run without --confirm: this copies into a production bucket. Nothing has been touched.'
fi

run_id="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}"
[[ "${run_id}" =~ ^[a-z0-9-]{1,40}$ ]] || fail 'the run identifier is not a valid name fragment'
name="wedding-backup-mirror-${run_id}"
readonly name

kube() {
  "${kubectl_bin}" --context "${context}" --namespace "${namespace}" "$@"
}

evaluator="${MIRROR_EVALUATOR:-}"
if [[ -z "${evaluator}" ]]; then
  evaluator="${work_dir}/evaluator"
  (cd "${root_dir}" && go build -o "${evaluator}" ./scripts/mirror-wedding-backup-catalogue) ||
    fail 'could not build the evaluator'
fi
readonly evaluator

archive_reference() {
  kube get cluster.postgresql.cnpg.io "${cluster}" \
    -o "jsonpath={.spec.plugins[?(@.name==\"${plugin}\")].parameters.barmanObjectName}"
}

require_shared_archive() {
  local reference
  reference="$(archive_reference)" || fail "could not read the ${cluster} Cluster"
  [[ "${reference}" == "${source_store}" ]] ||
    fail "the ${cluster} Cluster archives through '${reference}', not '${source_store}'. The mirror only runs before the cutover."
}

store_field() {
  kube get objectstores.barmancloud.cnpg.io "$1" -o "jsonpath={$2}"
}

# read_store <store> sets store_bucket, store_prefix, store_secret, store_endpoint.
read_store() {
  local store="$1" path access secret
  path="$(store_field "${store}" '.spec.configuration.destinationPath')" ||
    fail "could not read the ${store} ObjectStore"
  store_endpoint="$(store_field "${store}" '.spec.configuration.endpointURL')" ||
    fail "could not read the ${store} ObjectStore endpoint"
  access="$(store_field "${store}" '.spec.configuration.s3Credentials.accessKeyId.name')" ||
    fail "could not read the ${store} access key reference"
  secret="$(store_field "${store}" '.spec.configuration.s3Credentials.secretAccessKey.name')" ||
    fail "could not read the ${store} secret key reference"

  [[ "${path}" =~ ^s3://([a-z0-9][a-z0-9.-]*)/([A-Za-z0-9._/-]+)$ ]] ||
    fail "the ${store} ObjectStore destination is not an s3://<bucket>/<prefix> path"
  store_bucket="${BASH_REMATCH[1]}"
  store_prefix="${BASH_REMATCH[2]}"
  [[ -n "${access}" && "${access}" == "${secret}" ]] ||
    fail "the ${store} ObjectStore splits its credential across Secrets"
  store_secret="${access}"
  [[ "${store_endpoint}" == https://* ]] || fail "the ${store} ObjectStore endpoint is not https"
}

require_shared_archive

read_store "${source_store}"
readonly source_bucket="${store_bucket}" source_prefix="${store_prefix}" \
  source_secret="${store_secret}" source_endpoint="${store_endpoint}"
read_store "${destination_store}"
readonly destination_bucket="${store_bucket}" destination_prefix="${store_prefix}" \
  destination_secret="${store_secret}" destination_endpoint="${store_endpoint}"

[[ "${source_endpoint}" == "${destination_endpoint}" ]] ||
  fail 'the two ObjectStores name different endpoints'

"${evaluator}" validate-plan "${source_bucket}" "${source_prefix}" "${source_secret}" \
  "${destination_bucket}" "${destination_prefix}" "${destination_secret}" ||
  fail 'the live ObjectStores do not match the reviewed mirror plan'

created=true
kube create configmap "${name}" --from-file="mirror.sh=${pod_script}" >/dev/null ||
  fail 'could not stage the mirror script'

# Values are data in the manifest, never shell: the heredoc is quoted, so nothing
# in it expands, and each placeholder is replaced by a literal string below.
manifest="$(
  cat <<'MANIFEST'
apiVersion: v1
kind: Pod
metadata:
  name: __NAME__
  namespace: wedding-app
  labels:
    app.kubernetes.io/name: wedding-backup-mirror
    app.kubernetes.io/managed-by: mirror-wedding-backup-catalogue
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  activeDeadlineSeconds: 10800
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  volumes:
    - name: script
      configMap:
        name: __NAME__
    - name: source-credential
      secret:
        secretName: __SOURCE_SECRET__
        items:
          - key: ACCESS_KEY_ID
            path: ACCESS_KEY_ID
          - key: SECRET_ACCESS_KEY
            path: SECRET_ACCESS_KEY
    - name: destination-credential
      secret:
        secretName: __DESTINATION_SECRET__
        items:
          - key: ACCESS_KEY_ID
            path: ACCESS_KEY_ID
          - key: SECRET_ACCESS_KEY
            path: SECRET_ACCESS_KEY
    - name: work
      emptyDir:
        sizeLimit: 64Gi
  containers:
    - name: mirror
      image: __IMAGE__
      command: ["/bin/sh", "/mirror/mirror.sh"]
      securityContext:
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      env:
        - name: ENDPOINT
          value: "__ENDPOINT__"
        - name: SOURCE_BUCKET
          value: "__SOURCE_BUCKET__"
        - name: SOURCE_PREFIX
          value: "__SOURCE_PREFIX__"
        - name: DESTINATION_BUCKET
          value: "__DESTINATION_BUCKET__"
        - name: DESTINATION_PREFIX
          value: "__DESTINATION_PREFIX__"
        - name: WORK_DIR
          value: /work
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
        limits:
          memory: 512Mi
      volumeMounts:
        - name: script
          mountPath: /mirror
          readOnly: true
        - name: source-credential
          mountPath: /credentials/source
          readOnly: true
        - name: destination-credential
          mountPath: /credentials/destination
          readOnly: true
        - name: work
          mountPath: /work
MANIFEST
)"
manifest="${manifest//__NAME__/${name}}"
manifest="${manifest//__IMAGE__/${mc_image}}"
manifest="${manifest//__SOURCE_SECRET__/${source_secret}}"
manifest="${manifest//__DESTINATION_SECRET__/${destination_secret}}"
manifest="${manifest//__ENDPOINT__/${source_endpoint}}"
manifest="${manifest//__SOURCE_BUCKET__/${source_bucket}}"
manifest="${manifest//__SOURCE_PREFIX__/${source_prefix}}"
manifest="${manifest//__DESTINATION_BUCKET__/${destination_bucket}}"
manifest="${manifest//__DESTINATION_PREFIX__/${destination_prefix}}"

printf '%s\n' "${manifest}" | kube apply -f - >/dev/null || fail 'could not start the mirror pod'

phase=''
for ((attempt = 0; attempt < poll_limit; attempt++)); do
  phase="$(kube get pod "${name}" -o 'jsonpath={.status.phase}')" || phase=''
  [[ "${phase}" == Succeeded || "${phase}" == Failed ]] && break
  sleep "${poll_interval}"
done

kube logs "pod/${name}" >"${work_dir}/pod.log" 2>/dev/null || : >"${work_dir}/pod.log"
if [[ "${phase}" != Succeeded ]]; then
  grep '^mirror-pod: ' "${work_dir}/pod.log" >&2 || true
  fail "the mirror pod ended in phase '${phase:-unknown}'"
fi

# extract <section> writes exactly one BEGIN..END block, or refuses.
extract() {
  local section="$1" begins ends
  begins="$(grep -cxF "==== BEGIN ${section} ====" "${work_dir}/pod.log" || true)"
  ends="$(grep -cxF "==== END ${section} ====" "${work_dir}/pod.log" || true)"
  [[ "${begins}" == 1 && "${ends}" == 1 ]] ||
    fail "the mirror pod output does not carry exactly one ${section} listing"
  awk -v begin="==== BEGIN ${section} ====" -v end="==== END ${section} ====" '
    $0 == end { inside = 0 }
    inside { print }
    $0 == begin { inside = 1 }
  ' "${work_dir}/pod.log" >"${work_dir}/${section}.jsonl"
}

extract source-before
extract source-after
extract destination

run_start_lines="$(grep -c '^==== RUN-START .* ====$' "${work_dir}/pod.log" || true)"
[[ "${run_start_lines}" == 1 ]] || fail 'the mirror pod output does not carry exactly one run start'
run_start="$(sed -n 's/^==== RUN-START \(.*\) ====$/\1/p' "${work_dir}/pod.log")"

require_shared_archive

if ! "${evaluator}" evaluate "${run_start}" "${source_bucket}" \
  "${work_dir}/source-before.jsonl" "${work_dir}/source-after.jsonl" \
  "${work_dir}/destination.jsonl" >"${work_dir}/summary.json"; then
  fail 'the evaluator refused the mirror'
fi

# report_verdict prints the summary and exits with the documented status.
report_verdict() {
  cat "$1"
  if grep -q '"converged":true' "$1"; then
    printf 'CONVERGED: the dedicated bucket holds the full catalogue.\n'
    exit 0
  fi
  if grep -q '"converged":false' "$1"; then
    printf 'NOT CONVERGED: objects archived during the run are still pending. Run the mirror again.\n'
    exit 3
  fi
  fail 'the evaluator summary has no convergence verdict'
}

report_verdict "${work_dir}/summary.json"
