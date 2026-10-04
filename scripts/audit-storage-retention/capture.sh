#!/usr/bin/env bash
# Capture only read-only API responses. No output exists as complete until all reads succeed.
set -Eeuo pipefail
umask 077
if [[ $# != 2 || -z $1 || -z $2 ]]; then
  echo 'usage: capture.sh CONTEXT NEW_PRIVATE_DIRECTORY' >&2
  exit 2
fi
context=$1
directory=$2
mkdir -- "$directory"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Ordinary kubectl JSON flattens typed lists and loses their list resourceVersion.
# Read the fixed API endpoints directly so the auditor can verify completeness.
kubectl --context "$context" --request-timeout=30s get --raw /apis/storage.k8s.io/v1/storageclasses > "$directory/classes.json"
kubectl --context "$context" --request-timeout=30s get --raw /api/v1/persistentvolumes > "$directory/volumes.json"
kubectl --context "$context" --request-timeout=30s get --raw /api/v1/persistentvolumeclaims > "$directory/claims.json"
kubectl --context "$context" --request-timeout=30s get --raw /apis/velero.io/v2alpha1/datauploads > "$directory/uploads.json"
completed=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '{"startedAt":"%s","completedAt":"%s"}\n' "$started" "$completed" > "$directory/capture.json"
