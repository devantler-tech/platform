#!/usr/bin/env bash

# Mirror the shared Wedding backup catalogue into its dedicated bucket and prove
# the copy with the reviewed evaluator (scripts/mirror-wedding-backup-catalogue).
#
# This is the copy step #3252's cutover waits on: the Cluster may only switch its
# archive reference to wedding-db-dedicated once the dedicated bucket holds the
# full recoverable history, and the cutover is only complete once a catch-up pass
# has proven nothing was lost across the switch.
#
# TWO MODES.
#   --confirm                           mirror, before the switch
#   --confirm --catch-up <switch-time>  catch-up, after the switch (#3778)
#
# WHAT IT DOES, in order, refusing at the first thing it cannot prove:
#   1. Confirms the live Cluster archives through the store the mode expects:
#      `wedding-db` for the mirror, `wedding-db-dedicated` for the catch-up.
#   2. Reads both live ObjectStores and hands their exact bucket, prefix and
#      Secret names to `validate-plan`, so a drifted store is refused rather than
#      replaced with the reviewed value.
#   3. Runs scripts/mirror-wedding-backup-catalogue-pod.sh in a short-lived pod in
#      wedding-app, where both credentials and R2 egress already exist, and
#      collects its listings with `kubectl exec` once it reports them ready. The
#      copy walks only the shared catalogue's keys, so after the switch it can only
#      rewrite a destination object that shares a key with a shared object and
#      differs from it. Both stores write the same gzip-compressed segment names,
#      so such a key holds the same segment, and the evaluator checks its content.
#   4. For the catch-up, reads the Cluster's server directory and refuses if it
#      changes before the final check, because WAL continuity is only meaningful
#      within one server directory.
#   5. Confirms the Cluster STILL archives through that store.
#   6. Runs `evaluate` (mirror) or `evaluate-catch-up` (catch-up) on the three
#      listings.
#
# Exit status for the mirror: 0 converged (the destination is ready for the
# cutover), 3 copied and verified but not converged (objects archived during the
# run are still pending, so run it again), 1 refused or failed. Only 0 means the
# switch may start, and even 0 covers nothing archived after the run.
#
# Exit status for the catch-up: 0 caught up (every shared object is copied and the
# dedicated store's WAL continues the shared catalogue with no gap), 3 verified but
# not caught up (no segment has been archived through the dedicated store yet, so
# run it again after the next archive), 1 refused or failed.
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
readonly ready_marker='==== LISTINGS READY ===='
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

mode=''
switch_time=''
if [[ "$#" -eq 1 && "$1" == '--confirm' ]]; then
  mode=mirror
elif [[ "$#" -eq 3 && "$1" == '--confirm' && "$2" == '--catch-up' ]]; then
  mode=catch-up
  switch_time="$3"
else
  fail 'refusing to run: use --confirm, or --confirm --catch-up <switch-time>. This copies into a production bucket. Nothing has been touched.'
fi
readonly mode

if [[ "${mode}" == catch-up ]]; then
  [[ "${switch_time}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
    fail 'the switch time must be the UTC time the archive reference changed, as YYYY-MM-DDTHH:MM:SSZ. Nothing has been touched.'
  # The fixed-width UTC form orders lexically, so a string comparison is a time
  # comparison. A switch recorded in the future cannot have happened yet.
  [[ ! "${switch_time}" > "$(date -u +%Y-%m-%dT%H:%M:%SZ)" ]] ||
    fail 'the switch time is in the future. Nothing has been touched.'
fi
readonly switch_time

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

# The format check above accepts impossible dates such as February 30. Parse the
# switch time as the evaluator will, before anything touches the cluster.
if [[ "${mode}" == catch-up ]]; then
  "${evaluator}" validate-switch-time "${switch_time}" ||
    fail 'the switch time is not a valid UTC time. Nothing has been touched.'
fi

archive_reference() {
  kube get cluster.postgresql.cnpg.io "${cluster}" \
    -o "jsonpath={.spec.plugins[?(@.name==\"${plugin}\")].parameters.barmanObjectName}"
}

server_name_reference() {
  kube get cluster.postgresql.cnpg.io "${cluster}" \
    -o "jsonpath={.spec.plugins[?(@.name==\"${plugin}\")].parameters.serverName}"
}

# require_archive_reference refuses unless the Cluster archives through the store
# this mode depends on: the shared store before the cutover, the dedicated store
# after it. The catch-up also records the Cluster's server directory, because WAL
# continuity is only meaningful within it, and refuses if it changes mid-run.
server_name=''
require_archive_reference() {
  local reference current_server
  reference="$(archive_reference)" || fail "could not read the ${cluster} Cluster"
  if [[ "${mode}" == catch-up ]]; then
    [[ "${reference}" == "${destination_store}" ]] ||
      fail "the ${cluster} Cluster archives through '${reference}', not '${destination_store}'. The catch-up only runs after the cutover."
    current_server="$(server_name_reference)" || fail "could not read the ${cluster} Cluster server name"
    [[ "${current_server}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] ||
      fail "the ${cluster} Cluster names an invalid server directory"
    if [[ -z "${server_name}" ]]; then
      server_name="${current_server}"
    elif [[ "${current_server}" != "${server_name}" ]]; then
      fail "the ${cluster} Cluster server name changed from '${server_name}' to '${current_server}' during the catch-up"
    fi
  else
    [[ "${reference}" == "${source_store}" ]] ||
      fail "the ${cluster} Cluster archives through '${reference}', not '${source_store}'. The mirror only runs before the cutover."
  fi
}

store_field() {
  kube get objectstores.barmancloud.cnpg.io "$1" -o "jsonpath={$2}"
}

# read_store <store> sets store_bucket, store_prefix, store_secret, store_endpoint.
read_store() {
  local store="$1" path access secret access_key secret_key
  path="$(store_field "${store}" '.spec.configuration.destinationPath')" ||
    fail "could not read the ${store} ObjectStore"
  store_endpoint="$(store_field "${store}" '.spec.configuration.endpointURL')" ||
    fail "could not read the ${store} ObjectStore endpoint"
  access="$(store_field "${store}" '.spec.configuration.s3Credentials.accessKeyId.name')" ||
    fail "could not read the ${store} access key reference"
  secret="$(store_field "${store}" '.spec.configuration.s3Credentials.secretAccessKey.name')" ||
    fail "could not read the ${store} secret key reference"
  access_key="$(store_field "${store}" '.spec.configuration.s3Credentials.accessKeyId.key')" ||
    fail "could not read the ${store} access key field"
  secret_key="$(store_field "${store}" '.spec.configuration.s3Credentials.secretAccessKey.key')" ||
    fail "could not read the ${store} secret key field"

  [[ "${path}" =~ ^s3://([a-z0-9][a-z0-9.-]*)/([A-Za-z0-9._/-]+)$ ]] ||
    fail "the ${store} ObjectStore destination is not an s3://<bucket>/<prefix> path"
  store_bucket="${BASH_REMATCH[1]}"
  store_prefix="${BASH_REMATCH[2]}"
  [[ -n "${access}" && "${access}" == "${secret}" ]] ||
    fail "the ${store} ObjectStore splits its credential across Secrets"
  [[ "${access}" =~ ^[a-z0-9][a-z0-9.-]*$ ]] ||
    fail "the ${store} ObjectStore names an invalid Secret"
  store_secret="${access}"
  # The pod mounts exactly these two keys, so any other field name would leave it
  # waiting on a mount that can never succeed while it holds the deploy lock.
  [[ "${access_key}" == ACCESS_KEY_ID && "${secret_key}" == SECRET_ACCESS_KEY ]] ||
    fail "the ${store} ObjectStore reads its credential from keys the mirror does not mount"
  # The endpoint is written into the pod manifest, so it must be a bare host.
  [[ "${store_endpoint}" =~ ^https://[a-z0-9][a-z0-9.-]*$ ]] ||
    fail "the ${store} ObjectStore endpoint is not an https URL with a bare host"
}

require_archive_reference

read_store "${source_store}"
readonly source_bucket="${store_bucket}" source_prefix="${store_prefix}" \
  source_secret="${store_secret}" source_endpoint="${store_endpoint}"
read_store "${destination_store}"
readonly destination_bucket="${store_bucket}" destination_prefix="${store_prefix}" \
  destination_secret="${store_secret}" destination_endpoint="${store_endpoint}"

[[ "${source_endpoint}" == "${destination_endpoint}" ]] ||
  fail 'the two ObjectStores name different endpoints'

# Both credentials are about to be handed to this endpoint, so a live value is not
# trusted on its own: two drifted stores could agree on a foreign host, including
# another account's R2 host that the namespace egress already allows. Pin it to
# the endpoint committed in the reviewed bootstrap ConfigMap.
bootstrap_config="${MIRROR_BOOTSTRAP_CONFIG:-${root_dir}/k8s/bases/bootstrap/config-map.yaml}"
trusted_endpoints="$(sed -n 's/^  r2_endpoint: *//p' "${bootstrap_config}")" ||
  fail 'could not read the committed R2 endpoint'
[[ -n "${trusted_endpoints}" && "${trusted_endpoints}" != *$'\n'* ]] ||
  fail 'the bootstrap ConfigMap does not commit exactly one R2 endpoint'
[[ "${trusted_endpoints}" =~ ^https://[a-z0-9][a-z0-9.-]*$ ]] ||
  fail 'the committed R2 endpoint is not an https URL with a bare host'
[[ "${source_endpoint}" == "${trusted_endpoints}" ]] ||
  fail 'the live ObjectStores name an endpoint other than the committed R2 endpoint'

"${evaluator}" validate-plan "${source_bucket}" "${source_prefix}" "${source_secret}" \
  "${destination_bucket}" "${destination_prefix}" "${destination_secret}" ||
  fail 'the live ObjectStores do not match the reviewed mirror plan'

created=true
kube create configmap "${name}" --from-file="mirror.sh=${pod_script}" >/dev/null ||
  fail 'could not stage the mirror script'

# The heredoc is quoted, so nothing in it expands, and each placeholder is
# replaced by a value validated above. Every one of those values is restricted to
# characters that cannot end a YAML string or act as a replacement pattern (no
# quote, newline, backslash or `&`), which is what keeps the unquoted
# replacements below portable across bash 3.2 and 5.2.
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
    # `mc alias set` writes both credentials into its config directory. Keep that
    # in memory so the keys never land on node storage.
    - name: mc-config
      emptyDir:
        medium: Memory
        sizeLimit: 1Mi
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
        - name: MC_CONFIG_DIR
          value: /mc-config
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
        limits:
          memory: 1Gi
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
        - name: mc-config
          mountPath: /mc-config
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
ready=false
for ((attempt = 0; attempt < poll_limit; attempt++)); do
  phase="$(kube get pod "${name}" -o 'jsonpath={.status.phase}')" || phase=''
  [[ "${phase}" == Succeeded || "${phase}" == Failed ]] && break
  if [[ "${phase}" == Running ]] &&
    kube logs "pod/${name}" 2>/dev/null | grep -qxF "${ready_marker}"; then
    ready=true
    break
  fi
  sleep "${poll_interval}"
done

if [[ "${ready}" != true ]]; then
  kube logs "pod/${name}" 2>/dev/null | grep '^mirror-pod: ' >&2 || true
  fail "the mirror pod never reported its listings ready (phase '${phase:-unknown}')"
fi

for listing in run-start source-before source-after destination; do
  kube exec "${name}" -c mirror -- cat "/work/${listing}" >"${work_dir}/${listing}" ||
    fail "could not collect the ${listing} listing from the mirror pod"
done

run_start="$(cat "${work_dir}/run-start")"
[[ "${run_start}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
  fail 'the mirror pod reported a malformed run start'

require_archive_reference

if [[ "${mode}" == catch-up ]]; then
  if ! "${evaluator}" evaluate-catch-up "${switch_time}" "${server_name}" "${source_bucket}" \
    "${work_dir}/source-before" "${work_dir}/source-after" \
    "${work_dir}/destination" >"${work_dir}/summary.json"; then
    fail 'the evaluator refused the catch-up'
  fi
elif ! "${evaluator}" evaluate "${run_start}" "${source_bucket}" \
  "${work_dir}/source-before" "${work_dir}/source-after" \
  "${work_dir}/destination" >"${work_dir}/summary.json"; then
  fail 'the evaluator refused the mirror'
fi

# report_verdict prints the summary and exits with the documented status.
report_verdict() {
  cat "$1"
  if grep -q '"converged":true' "$1"; then
    if [[ "${mode}" == catch-up ]]; then
      printf 'CAUGHT UP: every shared object is in the dedicated bucket and its WAL continues the shared catalogue with no gap. The cutover is complete.\n'
    else
      printf 'CONVERGED: the dedicated bucket holds the full catalogue as of this run, so the switch may start.\n'
      printf 'WAL archived after this run is not covered: after the switch, run the catch-up (--catch-up <switch-time>).\n'
    fi
    exit 0
  fi
  if grep -q '"converged":false' "$1"; then
    if [[ "${mode}" == catch-up ]]; then
      printf 'NOT CAUGHT UP: no WAL segment newer than the shared catalogue has been archived through the dedicated store yet. Run the catch-up again after the next archive.\n'
    else
      printf 'NOT CONVERGED: objects archived during the run are still pending. Run the mirror again.\n'
    fi
    exit 3
  fi
  fail 'the evaluator summary has no convergence verdict'
}

report_verdict "${work_dir}/summary.json"
