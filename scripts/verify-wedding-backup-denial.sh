#!/usr/bin/env bash

# Prove that the dedicated Wedding backup credential is refused by the shared
# backup destination, using the credentials deployed in wedding-app (#3253).
#
# Shared recovery access is retired only after this proof passes: until the
# dedicated identity is observed being refused, its isolation rests on how the
# token was configured rather than on what the destination enforces.
#
# WHAT IT DOES, in order, refusing at the first thing it cannot prove:
#   1. Confirms the wedding-db Cluster archives through wedding-db-dedicated, so
#      the credential under test is the one the database actually uses.
#   2. Reads both live ObjectStores and requires the reviewed destination and
#      Secret for each. The shared bucket and the endpoint are pinned to the
#      committed bootstrap ConfigMap, because both credentials are about to be
#      handed to that endpoint and the shared bucket is the one under test.
#   3. Runs scripts/verify-wedding-backup-denial-pod.sh in a short-lived pod in
#      wedding-app, where both credentials and R2 egress already exist, and
#      accepts only its complete receipt.
#
# Exit status: 0 when list, read and write were each refused with AccessDenied;
# 1 when anything was refused, failed or not proven. Needs --confirm, because the
# proof attempts a write to a production bucket that must be refused.

set -euo pipefail

readonly context='admin@prod'
readonly namespace='wedding-app'
readonly cluster='wedding-db'
readonly shared_store='wedding-db'
readonly dedicated_store='wedding-db-dedicated'
readonly shared_secret='wedding-db-backup-r2'
readonly dedicated_secret='wedding-db-backup-r2-dedicated'
readonly catalogue_prefix='cnpg/wedding-db'
readonly dedicated_bucket='wedding-db-backups'
# The pod script names its probe objects under the same prefix; the test
# asserts the two stay identical.
readonly probe_prefix='wedding-backup-denial-probe'
readonly plugin='barman-cloud.cloudnative-pg.io'
readonly ready_marker='==== DENIAL OBSERVED ===='
readonly receipt='{"dedicatedCatalogueReachable":true,"sharedCatalogueReferenced":true,"listDenied":true,"readDenied":true,"writeDenied":true}'
# The same digest-pinned images as the catalogue mirror, whose runtime test
# exercises them; the denial test asserts the two scripts stay identical here.
readonly mc_image='quay.io/minio/aistor/mc:RELEASE.2026-03-12T04-18-55Z@sha256:6c33dc0fbf65c362be95003cd010ed95a41c556500833ea139f86de40c4c4e9f'
readonly tools_image='docker.io/library/busybox:1.38.0-musl@sha256:ea2b9914a16a4ac1981994af97b318f7c7d4db76b580c56177f08bf76f4a0be8'

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly pod_script="${root_dir}/scripts/verify-wedding-backup-denial-pod.sh"

kubectl_bin="${KUBECTL:-kubectl}"
readonly kubectl_bin
poll_interval="${DENIAL_POLL_INTERVAL:-10}"
poll_limit="${DENIAL_POLL_LIMIT:-90}"
readonly poll_interval poll_limit

work_dir="$(mktemp -d)"
readonly work_dir
# Cleanup removes only what this run created, never a same-named object it found.
created_configmap=false
created_pod=false
name=''
# Invoked by the EXIT trap below. shellcheck reports this trap-only handler as
# unused (SC2329 on 0.11) or unreachable (SC2317 on the older CI runner); the
# test suite asserts both deletions actually happen.
# shellcheck disable=SC2317,SC2329
cleanup() {
  if [[ "${created_pod}" == true ]]; then
    kube delete pod "${name}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  if [[ "${created_configmap}" == true ]]; then
    kube delete configmap "${name}" --ignore-not-found >/dev/null 2>&1 || true
  fi
  rm -rf "${work_dir}"
}
trap cleanup EXIT

fail() {
  printf 'verify-wedding-backup-denial: %s\n' "$1" >&2
  exit 1
}

[[ "$#" -eq 1 && "$1" == '--confirm' ]] ||
  fail 'refusing to run: use --confirm. The proof attempts a write to a production bucket that must be refused. Nothing has been touched.'

# A local run is named by its start time, so it never shares a name with an
# earlier run's pod.
run_id="${GITHUB_RUN_ID:-local$(date +%s)}-${GITHUB_RUN_ATTEMPT:-1}"
[[ "${run_id}" =~ ^[a-z0-9-]{1,40}$ ]] || fail 'the run identifier is not a valid name fragment'
name="wedding-backup-denial-${run_id}"
readonly name run_id

kube() {
  "${kubectl_bin}" --context "${context}" --namespace "${namespace}" --request-timeout=20s "$@"
}

# The committed bootstrap ConfigMap names the shared destination and the R2
# endpoint. A live value is not trusted on its own: two drifted stores could agree
# on a foreign host or bucket, including another account's R2 host that the
# namespace egress already allows.
bootstrap_config="${DENIAL_BOOTSTRAP_CONFIG:-${root_dir}/k8s/bases/bootstrap/config-map.yaml}"
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

kube get clusters.postgresql.cnpg.io "${cluster}" -o json >"${work_dir}/cluster.json" 2>/dev/null ||
  fail "could not read the ${cluster} Cluster"
jq -e --arg plugin "${plugin}" --arg store "${dedicated_store}" '
  .metadata.deletionTimestamp == null and
  ([.spec.plugins[]? | select(.name == $plugin)] |
    length == 1 and .[0].enabled == true and .[0].parameters.barmanObjectName == $store)
' "${work_dir}/cluster.json" >/dev/null 2>&1 ||
  fail "the ${cluster} Cluster does not archive through ${dedicated_store}, so its credential is not the one in use"

# require_store <store> <bucket> <secret> refuses unless the live ObjectStore
# writes to the reviewed destination through exactly the reviewed Secret keys the
# pod mounts, at the committed endpoint.
require_store() {
  local store="$1" bucket="$2" secret="$3"
  kube get objectstores.barmancloud.cnpg.io "${store}" -o json >"${work_dir}/${store}.json" 2>/dev/null ||
    fail "could not read the ${store} ObjectStore"
  jq -e --arg path "s3://${bucket}/${catalogue_prefix}" --arg endpoint "${endpoint}" --arg secret "${secret}" '
    .metadata.deletionTimestamp == null and
    .spec.configuration.destinationPath == $path and
    .spec.configuration.endpointURL == $endpoint and
    .spec.configuration.s3Credentials.accessKeyId == {name:$secret,key:"ACCESS_KEY_ID"} and
    .spec.configuration.s3Credentials.secretAccessKey == {name:$secret,key:"SECRET_ACCESS_KEY"}
  ' "${work_dir}/${store}.json" >/dev/null 2>&1 ||
    fail "the ${store} ObjectStore is not wired to the reviewed destination and credential"
}
require_store "${shared_store}" "${shared_bucket}" "${shared_secret}"
require_store "${dedicated_store}" "${dedicated_bucket}" "${dedicated_secret}"

kube create configmap "${name}" --from-file="denial.sh=${pod_script}" >/dev/null ||
  fail 'could not stage the denial proof script'
created_configmap=true

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
    app.kubernetes.io/name: wedding-backup-denial
    app.kubernetes.io/managed-by: verify-wedding-backup-denial
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  activeDeadlineSeconds: 1800
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
    - name: shared-credential
      secret:
        secretName: __SHARED_SECRET__
        items:
          - key: ACCESS_KEY_ID
            path: ACCESS_KEY_ID
          - key: SECRET_ACCESS_KEY
            path: SECRET_ACCESS_KEY
    - name: dedicated-credential
      secret:
        secretName: __DEDICATED_SECRET__
        items:
          - key: ACCESS_KEY_ID
            path: ACCESS_KEY_ID
          - key: SECRET_ACCESS_KEY
            path: SECRET_ACCESS_KEY
    - name: work
      emptyDir:
        sizeLimit: 1Gi
    # `mc alias set` writes both credentials into its config directory. Keep that
    # in memory so the keys never land on node storage.
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
    - name: probe
      image: __IMAGE__
      command: ["/tools/sh", "/denial/denial.sh"]
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
        - name: SHARED_BUCKET
          value: "__SHARED_BUCKET__"
        - name: SHARED_PREFIX
          value: "__PREFIX__"
        - name: DEDICATED_BUCKET
          value: "__DEDICATED_BUCKET__"
        - name: DEDICATED_PREFIX
          value: "__PREFIX__"
        - name: PROBE_ID
          value: "__PROBE_ID__"
        - name: WORK_DIR
          value: /work
        - name: MC_CONFIG_DIR
          value: /mc-config
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
        limits:
          memory: 256Mi
      volumeMounts:
        - name: tools
          mountPath: /tools
          readOnly: true
        - name: script
          mountPath: /denial
          readOnly: true
        - name: shared-credential
          mountPath: /credentials/shared
          readOnly: true
        - name: dedicated-credential
          mountPath: /credentials/dedicated
          readOnly: true
        - name: work
          mountPath: /work
        - name: mc-config
          mountPath: /mc-config
MANIFEST
)"
manifest="${manifest//__NAME__/${name}}"
manifest="${manifest//__IMAGE__/${mc_image}}"
manifest="${manifest//__TOOLS_IMAGE__/${tools_image}}"
manifest="${manifest//__SHARED_SECRET__/${shared_secret}}"
manifest="${manifest//__DEDICATED_SECRET__/${dedicated_secret}}"
manifest="${manifest//__ENDPOINT__/${endpoint}}"
manifest="${manifest//__SHARED_BUCKET__/${shared_bucket}}"
manifest="${manifest//__DEDICATED_BUCKET__/${dedicated_bucket}}"
manifest="${manifest//__PREFIX__/${catalogue_prefix}}"
manifest="${manifest//__PROBE_ID__/${run_id}}"

# create, never apply: an earlier pod with this name must not have its old
# phase and log read as this run's result.
printf '%s\n' "${manifest}" | kube create -f - >/dev/null || fail 'could not start the denial proof pod'
created_pod=true

phase=''
for ((attempt = 0; attempt < poll_limit; attempt++)); do
  phase="$(kube get pod "${name}" -o 'jsonpath={.status.phase}' 2>/dev/null)" || phase=''
  [[ "${phase}" == Succeeded || "${phase}" == Failed ]] && break
  sleep "${poll_interval}"
done

kube logs "pod/${name}" -c probe >"${work_dir}/log" 2>/dev/null || : >"${work_dir}/log"
if [[ "${phase}" != Succeeded ]]; then
  grep '^denial-pod: ' "${work_dir}/log" >&2 || true
  # The pod removes a probe object that landed, but not if it was stopped first.
  fail "the denial proof pod did not succeed (phase '${phase:-unknown}'). If it reached the write, check ${shared_bucket}/${probe_prefix}/${run_id}; the next run refuses while that prefix exists"
fi

# Only the exact receipt immediately followed by the marker, as the final two
# lines, is a pass: a partial or reordered log is not evidence.
[[ "$(tail -n 2 "${work_dir}/log")" == "${receipt}"$'\n'"${ready_marker}" ]] ||
  fail 'the denial proof pod succeeded without its complete receipt'

printf '%s\n' "${receipt}"
printf 'DENIAL OBSERVED: the dedicated Wedding backup credential reaches its own catalogue, and the shared destination refused its list, read and write with AccessDenied.\n'
