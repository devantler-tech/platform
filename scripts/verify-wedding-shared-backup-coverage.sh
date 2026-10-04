#!/usr/bin/env bash

# Prove that the dedicated Wedding backup bucket covers everything the stale
# copy of the catalogue in the shared backup bucket still holds (#4481).
#
# Until the cutover the Wedding database archived into the shared backup bucket.
# That copy was mirrored into the dedicated bucket, and #3253 removed the store
# that pointed at it, so nothing applies retention to it any more and every
# holder of the shared backup credential can still read it. Removing it is
# irreversible, so the decision needs current evidence that nothing would be
# lost. This is that evidence, and it is all this does.
#
# READ ONLY. It lists both buckets and reads the objects it has to hash. It
# never writes to or deletes from either bucket.
#
# WHAT IT DOES, in order, refusing at the first thing it cannot prove:
#   1. Confirms the wedding-db Cluster archives through wedding-db-dedicated with
#      a healthy WAL archiver, that the wedding-app namespace no longer holds the
#      shared store or its credential, and that no ObjectStore or Cluster anywhere
#      still names the shared catalogue.
#   2. Reads the dedicated ObjectStore and the Umami ObjectStore, whose Secret is
#      the shared backup credential, and requires the reviewed destinations at the
#      committed endpoint and bucket.
#   3. Starts one pod beside each credential (scripts/verify-wedding-shared-backup-
#      coverage-pod.sh). No namespace holds both since #3253. The shared pod lists
#      first; the dedicated pod lists afterwards.
#   4. Has both pods hash every object an ETag cannot prove, and runs the reviewed
#      evaluator (`evaluate-coverage`): every shared object must be in the
#      dedicated catalogue with matching content, or be older than what the
#      dedicated store's retention still keeps.
#   5. Repeats step 1.
#
# Exit status: 0 covered; 1 refused, failed or not covered. Needs --confirm,
# because it starts pods beside two production backup credentials.

set -euo pipefail

readonly context='admin@prod'
readonly tenant_namespace='wedding-app'
readonly shared_namespace='umami'
readonly cluster='wedding-db'
readonly retired_store='wedding-db'
readonly retired_secret='wedding-db-backup-r2'
readonly dedicated_store='wedding-db-dedicated'
readonly dedicated_secret='wedding-db-backup-r2-dedicated'
readonly dedicated_bucket='wedding-db-backups'
readonly shared_reference_store='umami-db'
readonly shared_reference_prefix='cnpg/umami-db'
readonly shared_secret='umami-db-backup-r2'
readonly catalogue_prefix='cnpg/wedding-db'
readonly plugin='barman-cloud.cloudnative-pg.io'
readonly ready_marker='==== COVERAGE POD READY ===='
# The same digest-pinned images as the catalogue mirror, whose runtime test
# exercises them; the coverage test asserts the two scripts stay identical here.
readonly mc_image='quay.io/minio/aistor/mc:RELEASE.2026-03-12T04-18-55Z@sha256:6c33dc0fbf65c362be95003cd010ed95a41c556500833ea139f86de40c4c4e9f'
readonly tools_image='docker.io/library/busybox:1.38.0-musl@sha256:ea2b9914a16a4ac1981994af97b318f7c7d4db76b580c56177f08bf76f4a0be8'

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly pod_script="${root_dir}/scripts/verify-wedding-shared-backup-coverage-pod.sh"

kubectl_bin="${KUBECTL:-kubectl}"
readonly kubectl_bin
poll_interval="${COVERAGE_POLL_INTERVAL:-10}"
poll_limit="${COVERAGE_POLL_LIMIT:-60}"
readonly poll_interval poll_limit

work_dir="$(mktemp -d)"
readonly work_dir
# Cleanup removes only what this run created, never a same-named object it found.
created=()
name=''
# Invoked by the EXIT trap below. shellcheck reports this trap-only handler as
# unused (SC2329 on 0.11) or unreachable (SC2317 on the older CI runner); the
# test suite asserts the deletions actually happen.
# shellcheck disable=SC2317,SC2329
cleanup() {
  local entry
  for entry in ${created[@]+"${created[@]}"}; do
    kube "${entry%%/*}" delete "${entry#*/}" "${name}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  rm -rf "${work_dir}"
}
trap cleanup EXIT

fail() {
  printf 'verify-wedding-shared-backup-coverage: %s\n' "$1" >&2
  exit 1
}

[[ "$#" -eq 1 && "$1" == '--confirm' ]] ||
  fail 'refusing to run: use --confirm. The proof starts pods beside two production backup credentials. Nothing has been touched.'

# A local run is named by its start time, so it never shares a name with an
# earlier run's pod.
run_id="${GITHUB_RUN_ID:-local$(date +%s)}-${GITHUB_RUN_ATTEMPT:-1}"
[[ "${run_id}" =~ ^[a-z0-9-]{1,40}$ ]] || fail 'the run identifier is not a valid name fragment'
name="wedding-backup-coverage-${run_id}"
readonly name

# kube <namespace> <kubectl arguments...>
kube() {
  local namespace="$1"
  shift
  "${kubectl_bin}" --context "${context}" --namespace "${namespace}" --request-timeout=20s "$@"
}

evaluator="${COVERAGE_EVALUATOR:-}"
if [[ -z "${evaluator}" ]]; then
  evaluator="${work_dir}/evaluator"
  (cd "${root_dir}" && go build -o "${evaluator}" ./scripts/mirror-wedding-backup-catalogue) ||
    fail 'could not build the evaluator'
fi
readonly evaluator

# The committed bootstrap ConfigMap names the shared bucket and the R2 endpoint.
# A live value is not trusted on its own: a drifted store could name a foreign
# host or bucket, and a credential is about to be handed to that endpoint.
bootstrap_config="${COVERAGE_BOOTSTRAP_CONFIG:-${root_dir}/k8s/bases/bootstrap/config-map.yaml}"
committed() {
  local values
  values="$(sed -n "s/^  $1: *//p" "${bootstrap_config}")" || fail "could not read the committed $1"
  [[ -n "${values}" && "${values}" != *$'\n'* ]] || fail "the bootstrap ConfigMap does not commit exactly one $1"
  printf '%s' "${values}"
}
endpoint="$(committed r2_endpoint)"
shared_bucket="$(committed r2_bucket)"
readonly endpoint shared_bucket
[[ "${endpoint}" =~ ^https://[a-z0-9][a-z0-9.-]*$ ]] ||
  fail 'the committed R2 endpoint is not an https URL with a bare host'
[[ "${shared_bucket}" =~ ^[a-z0-9][a-z0-9.-]*$ ]] ||
  fail 'the committed shared backup bucket is not a valid bucket name'
[[ "${shared_bucket}" != "${dedicated_bucket}" ]] ||
  fail 'the committed shared backup bucket is the dedicated bucket'
readonly shared_catalogue="s3://${shared_bucket}/${catalogue_prefix}"

# require_cluster refuses unless the Cluster archives through the dedicated store
# with a healthy active WAL archiver, and is the same Cluster as on the first call.
cluster_uid=''
require_cluster() {
  local uid
  kube "${tenant_namespace}" get clusters.postgresql.cnpg.io "${cluster}" -o json >"${work_dir}/cluster.json" 2>/dev/null ||
    fail "could not read the ${cluster} Cluster"
  jq -e --arg plugin "${plugin}" --arg store "${dedicated_store}" '
    .metadata.generation as $generation |
    def healthy($kind):
      [.status.conditions[]? | select(.type == $kind)] |
      length == 1 and .[0].status == "True" and
      (.[0].observedGeneration == null or .[0].observedGeneration == $generation);
    .metadata.deletionTimestamp == null and
    (.metadata.uid | type == "string" and length > 0) and
    (.spec.instances | type == "number" and . > 0 and floor == .) and
    .status.readyInstances == .spec.instances and
    ([.spec.plugins[]? | select(.name == $plugin)] |
      length == 1 and .[0].enabled == true and .[0].isWALArchiver == true and
      .[0].parameters.barmanObjectName == $store) and
    healthy("Ready") and healthy("ContinuousArchiving")
  ' "${work_dir}/cluster.json" >/dev/null 2>&1 ||
    fail "the ${cluster} Cluster does not archive through ${dedicated_store} with a healthy active WAL archiver"
  uid="$(jq -r '.metadata.uid' "${work_dir}/cluster.json")"
  if [[ -z "${cluster_uid}" ]]; then
    cluster_uid="${uid}"
  elif [[ "${uid}" != "${cluster_uid}" ]]; then
    fail "the ${cluster} Cluster was replaced during the run"
  fi
}

# require_absent <resource> <name> refuses unless the wedding-app namespace is
# observed not to hold the object. A failed read is not an absence.
require_absent() {
  local found
  found="$(kube "${tenant_namespace}" get "$1" "$2" --ignore-not-found --output=name 2>/dev/null)" ||
    fail "could not check whether ${tenant_namespace} still holds $1/$2"
  [[ -z "${found}" ]] ||
    fail "${tenant_namespace} still holds $1/$2: shared backup access has not been retired (#3253)"
}

# require_unreferenced refuses when any ObjectStore or Cluster in the cluster
# still names the shared catalogue or a directory above it, in any field: something could then still be
# writing to it, and the coverage shown here would not last.
require_unreferenced() {
  local kind
  for kind in objectstores.barmancloud.cnpg.io clusters.postgresql.cnpg.io; do
    "${kubectl_bin}" --context "${context}" --request-timeout=20s get "${kind}" --all-namespaces -o json \
      >"${work_dir}/references.json" 2>/dev/null ||
      fail "could not list every ${kind}"
    jq -e --arg path "${shared_catalogue}" '
      (.items | type == "array") and
      ([.items[].spec | .. | strings | select(. == $path or startswith($path + "/") or (. as $root | $path | startswith($root + "/")))] | length == 0)
    ' "${work_dir}/references.json" >/dev/null 2>&1 ||
      fail "a ${kind} still names the shared Wedding catalogue"
  done
}

require_retired() {
  require_cluster
  require_absent objectstores.barmancloud.cnpg.io "${retired_store}"
  require_absent externalsecrets.external-secrets.io "${retired_secret}"
  require_absent secrets "${retired_secret}"
  require_unreferenced
}

# require_store <namespace> <store> <destination> <secret> refuses unless the
# live ObjectStore writes to the reviewed destination through exactly the
# reviewed Secret keys the pod mounts, at the committed endpoint.
require_store() {
  local namespace="$1" store="$2" destination="$3" secret="$4"
  kube "${namespace}" get objectstores.barmancloud.cnpg.io "${store}" -o json >"${work_dir}/store.json" 2>/dev/null ||
    fail "could not read the ${store} ObjectStore"
  jq -e --arg path "${destination}" --arg endpoint "${endpoint}" --arg secret "${secret}" '
    .metadata.deletionTimestamp == null and
    .spec.configuration.destinationPath == $path and
    .spec.configuration.endpointURL == $endpoint and
    .spec.configuration.s3Credentials.accessKeyId == {name:$secret,key:"ACCESS_KEY_ID"} and
    .spec.configuration.s3Credentials.secretAccessKey == {name:$secret,key:"SECRET_ACCESS_KEY"}
  ' "${work_dir}/store.json" >/dev/null 2>&1 ||
    fail "the ${store} ObjectStore is not wired to the reviewed destination and credential"
}

require_retired
require_store "${tenant_namespace}" "${dedicated_store}" "s3://${dedicated_bucket}/${catalogue_prefix}" "${dedicated_secret}"
# The evaluator accepts a shared object the dedicated store lacks only when it
# is older than this retention window, so the live store must declare the same one.
jq -e '.spec.retentionPolicy == "30d"' "${work_dir}/store.json" >/dev/null 2>&1 ||
  fail "the ${dedicated_store} ObjectStore does not keep the reviewed 30-day retention"
# The Umami store proves which Secret holds the credential for the committed
# shared bucket. Its own catalogue is a sibling of the Wedding one and is never
# listed.
require_store "${shared_namespace}" "${shared_reference_store}" "s3://${shared_bucket}/${shared_reference_prefix}" "${shared_secret}"

# start_pod <namespace> <role> <secret> <bucket>
start_pod() {
  local namespace="$1" role="$2" secret="$3" bucket="$4" manifest
  kube "${namespace}" create configmap "${name}" --from-file="coverage.sh=${pod_script}" >/dev/null ||
    fail "could not stage the coverage script in ${namespace}"
  created+=("${namespace}/configmap")

  # The heredoc is quoted, so nothing in it expands, and each placeholder is
  # replaced by a value validated above. Every one of those values is restricted
  # to characters that cannot end a YAML string or act as a replacement pattern
  # (no quote, newline, backslash or `&`), which is what keeps the unquoted
  # replacements below portable across bash 3.2 and 5.2.
  manifest="$(
    cat <<'MANIFEST'
apiVersion: v1
kind: Pod
metadata:
  name: __NAME__
  namespace: __NAMESPACE__
  labels:
    app.kubernetes.io/name: wedding-backup-coverage
    app.kubernetes.io/managed-by: verify-wedding-shared-backup-coverage
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  activeDeadlineSeconds: 3600
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  volumes:
    - name: tools
      emptyDir:
        sizeLimit: 8Mi
    - name: script
      configMap:
        name: __NAME__
    - name: credential
      secret:
        secretName: __SECRET__
        items:
          - key: ACCESS_KEY_ID
            path: ACCESS_KEY_ID
          - key: SECRET_ACCESS_KEY
            path: SECRET_ACCESS_KEY
    - name: work
      emptyDir:
        sizeLimit: 1Gi
    # `mc alias set` writes the credential into its config directory. Keep that
    # in memory so the key never lands on node storage.
    - name: mc-config
      emptyDir:
        medium: Memory
        sizeLimit: 1Mi
  initContainers:
    - name: install-tools
      image: __TOOLS_IMAGE__
      command:
        - /bin/sh
        - -ec
        - cp /bin/busybox /tools/busybox; /tools/busybox --install -s /tools
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      resources:
        requests:
          cpu: 10m
          memory: 8Mi
        limits:
          cpu: 100m
          memory: 32Mi
      volumeMounts:
        - name: tools
          mountPath: /tools
  containers:
    - name: coverage
      image: __IMAGE__
      command: ["/tools/sh", "/coverage/coverage.sh", "serve"]
      securityContext:
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      env:
        - name: PATH
          value: /tools:/usr/local/bin:/usr/bin:/bin
        - name: ENDPOINT
          value: "__ENDPOINT__"
        - name: ROLE
          value: "__ROLE__"
        - name: BUCKET
          value: "__BUCKET__"
        - name: PREFIX
          value: "__PREFIX__"
        - name: CREDENTIALS_DIR
          value: /credentials
        - name: WORK_DIR
          value: /work
        - name: MC_CONFIG_DIR
          value: /mc-config
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
        limits:
          memory: 512Mi
      volumeMounts:
        - name: tools
          mountPath: /tools
          readOnly: true
        - name: script
          mountPath: /coverage
          readOnly: true
        - name: credential
          mountPath: /credentials
          readOnly: true
        - name: work
          mountPath: /work
        - name: mc-config
          mountPath: /mc-config
MANIFEST
  )"
  manifest="${manifest//__NAME__/${name}}"
  manifest="${manifest//__NAMESPACE__/${namespace}}"
  manifest="${manifest//__IMAGE__/${mc_image}}"
  manifest="${manifest//__TOOLS_IMAGE__/${tools_image}}"
  manifest="${manifest//__SECRET__/${secret}}"
  manifest="${manifest//__ENDPOINT__/${endpoint}}"
  manifest="${manifest//__ROLE__/${role}}"
  manifest="${manifest//__BUCKET__/${bucket}}"
  manifest="${manifest//__PREFIX__/${catalogue_prefix}}"

  printf '%s\n' "${manifest}" | kube "${namespace}" create -f - >/dev/null ||
    fail "could not start the ${role} pod"
  created+=("${namespace}/pod")
}

# wait_ready <namespace> <role> waits for the pod to store its credential.
wait_ready() {
  local namespace="$1" role="$2" phase='' attempt
  for ((attempt = 0; attempt < poll_limit; attempt++)); do
    phase="$(kube "${namespace}" get pod "${name}" -o 'jsonpath={.status.phase}')" || phase=''
    [[ "${phase}" == Succeeded || "${phase}" == Failed ]] && break
    if [[ "${phase}" == Running ]] &&
      kube "${namespace}" logs "pod/${name}" -c coverage 2>/dev/null | grep -qxF "${ready_marker}"; then
      return 0
    fi
    sleep "${poll_interval}"
  done
  kube "${namespace}" logs "pod/${name}" -c coverage 2>/dev/null | grep '^coverage-pod: ' >&2 || true
  fail "the ${role} pod never became ready (phase '${phase:-unknown}')"
}

# in_pod <namespace> <command> runs one command of the pod script. The long
# request timeout covers hashing a base backup.
in_pod() {
  "${kubectl_bin}" --context "${context}" --namespace "$1" --request-timeout=45m \
    exec -i "${name}" -c coverage -- /tools/sh /coverage/coverage.sh "$2"
}

# collect <namespace> <pod file> <local file>
collect() {
  "${kubectl_bin}" --context "${context}" --namespace "$1" --request-timeout=10m \
    exec "${name}" -c coverage -- cat "/work/$2" >"${work_dir}/$3" ||
    fail "could not collect $2 from the pod in $1"
}

start_pod "${shared_namespace}" shared "${shared_secret}" "${shared_bucket}"
start_pod "${tenant_namespace}" dedicated "${dedicated_secret}" "${dedicated_bucket}"
wait_ready "${shared_namespace}" shared
wait_ready "${tenant_namespace}" dedicated

# The dedicated listing is taken after the shared one, because the evaluator
# refuses a dedicated listing that started earlier.
in_pod "${shared_namespace}" list </dev/null || fail 'the shared pod could not list the shared catalogue'
in_pod "${tenant_namespace}" list </dev/null || fail 'the dedicated pod could not list the dedicated catalogue'
collect "${shared_namespace}" listing shared
collect "${tenant_namespace}" listing dedicated

# A shared listing holding only its completion record means the copy is gone.
# That is a result in its own right, and the evaluator would refuse it as an
# empty source.
if [[ "$(wc -l <"${work_dir}/shared" | tr -d ' ')" == 1 ]] &&
  grep -Eqx "\\{\"status\":\"success\",\"type\":\"listing-complete\",\"location\":\"${shared_bucket}/${catalogue_prefix}\",\"started\":\"[0-9TZ:-]+\",\"files\":0\\}" "${work_dir}/shared"; then
  require_retired
  printf 'EMPTY: the shared bucket holds no Wedding catalogue. There is nothing left to cover.\n'
  exit 0
fi

"${evaluator}" coverage-multipart-keys "${shared_bucket}" "${work_dir}/shared" "${work_dir}/dedicated" \
  >"${work_dir}/multipart-keys" || fail 'the evaluator refused the listings'
in_pod "${shared_namespace}" hash <"${work_dir}/multipart-keys" || fail 'the shared pod could not hash its objects'
in_pod "${tenant_namespace}" hash <"${work_dir}/multipart-keys" || fail 'the dedicated pod could not hash its objects'
collect "${shared_namespace}" sums shared-sums
collect "${tenant_namespace}" sums dedicated-sums

"${evaluator}" evaluate-coverage "${shared_bucket}" "${work_dir}/shared" "${work_dir}/dedicated" \
  "${work_dir}/shared-sums" "${work_dir}/dedicated-sums" >"${work_dir}/summary.json" ||
  fail 'NOT COVERED: the dedicated catalogue does not hold everything the shared copy holds. Keep the shared copy.'
jq -e '.covered == true and (.sharedObjects | type == "number" and . > 0)' \
  "${work_dir}/summary.json" >/dev/null 2>&1 ||
  fail 'the evaluator summary is not a covered verdict'

require_retired

cat "${work_dir}/summary.json"
printf 'COVERED: of the %s objects in the shared Wedding catalogue, the dedicated bucket holds %s with matching content.\n' \
  "$(jq -r '.sharedObjects' "${work_dir}/summary.json")" "$(jq -r '.matchedObjects' "${work_dir}/summary.json")"
printf 'The other %s predate both the oldest backup the dedicated store keeps and its 30-day retention window, so retention has removed them there.\n' \
  "$(jq -r '.retentionPrunedObjects' "${work_dir}/summary.json")"
printf 'Nothing was changed. This holds as of this run: the shared copy is no longer written to.\n'
