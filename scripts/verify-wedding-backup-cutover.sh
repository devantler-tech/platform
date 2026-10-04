#!/usr/bin/env bash
# Create one online backup after the dedicated archive is active. Keep its
# Backup receipt for the catalogue and dedicated-only restore acceptance checks.
set -euo pipefail
fail() { printf 'Wedding backup proof refused: %s\n' "$*" >&2; exit 1; }
[[ "$#" == 0 && "${WEDDING_BACKUP_CONFIRM:-}" == verify-wedding-backup-cutover ]] || fail 'explicit confirmation is required'
[[ "${GITHUB_REPOSITORY:-}" == devantler-tech/platform && "${GITHUB_EVENT_NAME:-}" == workflow_dispatch && "${GITHUB_REF:-}" == refs/heads/main && "${GITHUB_RUN_ATTEMPT:-}" == 1 ]] || fail 'a first-attempt main dispatch is required'
[[ "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]*$ ]] || fail 'a workflow run identity is required'
readonly backup="wedding-db-dedicated-proof-${GITHUB_RUN_ID}"
umask 077
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
kube() { kubectl --context admin@prod --namespace wedding-app --request-timeout=20s "$@"; }
read_object() { kube get "$1" "$2" -o json >"$3" 2>/dev/null || fail 'a required object could not be read'; }
check_cluster() {
  jq -e '
    .apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster" and
    .metadata.name == "wedding-db" and .metadata.namespace == "wedding-app" and
    (.metadata.deletionTimestamp == null) and
    (.metadata.uid | test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")) and
    (.metadata.generation > 0) and (.spec.instances == 3) and (.status.readyInstances == .spec.instances) and
    ([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length == 1) and
    ([.status.conditions[]? | select(.type == "ContinuousArchiving" and .status == "True")] | length == 1) and
    ([.spec.plugins[]? | select(.name == "barman-cloud.cloudnative-pg.io")] |
      length == 1 and .[0].enabled == true and .[0].isWALArchiver == true and
      .[0].parameters.barmanObjectName == "wedding-db-dedicated" and
      .[0].parameters.serverName == "wedding-db-20260909")
  ' "$1" >/dev/null 2>&1 || fail 'the healthy dedicated archive is not active'
}
check_store() {
  jq -e '
    .apiVersion == "barmancloud.cnpg.io/v1" and .kind == "ObjectStore" and
    .metadata.name == "wedding-db-dedicated" and .metadata.namespace == "wedding-app" and
    (.metadata.deletionTimestamp == null) and
    (.spec.configuration.destinationPath == "s3://wedding-db-backups/cnpg/wedding-db") and
    (.spec.configuration.s3Credentials == {
      accessKeyId:{name:"wedding-db-backup-r2-dedicated",key:"ACCESS_KEY_ID"},
      secretAccessKey:{name:"wedding-db-backup-r2-dedicated",key:"SECRET_ACCESS_KEY"},
      region:{name:"wedding-db-backup-r2-dedicated",key:"REGION"}})
  ' "$1" >/dev/null 2>&1 || fail 'the dedicated store is not independently wired'
}
read_object clusters.postgresql.cnpg.io wedding-db "$tmp/before.json"
check_cluster "$tmp/before.json"
read_object objectstores.barmancloud.cnpg.io wedding-db-dedicated "$tmp/store.json"
check_store "$tmp/store.json"
cluster_uid="$(jq -r '.metadata.uid' "$tmp/before.json")"
generation="$(jq -r '.metadata.generation' "$tmp/before.json")"
jq -n --arg name "$backup" '{apiVersion:"postgresql.cnpg.io/v1",kind:"Backup",metadata:{name:$name,namespace:"wedding-app",labels:{"platform.devantler.tech/backup-proof":"dedicated-cutover"}},spec:{cluster:{name:"wedding-db"},method:"plugin",pluginConfiguration:{name:"barman-cloud.cloudnative-pg.io"},target:"primary"}}' >"$tmp/request.json"
# create, never apply: a rerun cannot mutate or substitute an older Backup.
kube create -f "$tmp/request.json" -o json >"$tmp/created.json" 2>/dev/null || fail 'the fresh backup could not be created'
backup_uid="$(jq -er '.metadata.uid | select(test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$"))' "$tmp/created.json")" || fail 'the created backup has no valid identity'
for ((attempt=1; attempt<=240; attempt++)); do
  read_object backups.postgresql.cnpg.io "$backup" "$tmp/backup.json"
  jq -e --arg uid "$backup_uid" --arg name "$backup" '
    .apiVersion == "postgresql.cnpg.io/v1" and .kind == "Backup" and
    .metadata.uid == $uid and .metadata.name == $name and .metadata.namespace == "wedding-app" and
    .metadata.deletionTimestamp == null and .spec == {cluster:{name:"wedding-db"},method:"plugin",pluginConfiguration:{name:"barman-cloud.cloudnative-pg.io"},target:"primary"}
  ' "$tmp/backup.json" >/dev/null 2>&1 || fail 'the backup identity or request changed'
  phase="$(jq -r '.status.phase // "pending"' "$tmp/backup.json")"
  case "$phase" in
    completed) break ;;
    failed) fail 'the fresh backup failed' ;;
    pending|started|running|finalizing) ;;
    *) fail 'the backup reported an unknown state' ;;
  esac
  [[ "$attempt" != 240 ]] || fail 'the backup did not complete within twenty minutes'
  sleep 5
done
jq -e --arg uid "$cluster_uid" '
  .status.phase == "completed" and .status.method == "plugin" and
  .status.pluginMetadata.clusterUID == $uid and
  .status.pluginMetadata.name == "barman-cloud.cloudnative-pg.io" and
  (.status.backupId | test("^[0-9]{8}T[0-9]{6}$")) and
  (.status.endWal | test("^[A-F0-9]{24}$")) and
  (.metadata.creationTimestamp | fromdateiso8601) <= (.status.startedAt | fromdateiso8601) and
  (.status.startedAt | fromdateiso8601) <= (.status.stoppedAt | fromdateiso8601)
' "$tmp/backup.json" >/dev/null 2>&1 || fail 'the backup completion receipt is incomplete or stale'
read_object clusters.postgresql.cnpg.io wedding-db "$tmp/after.json"
check_cluster "$tmp/after.json"
jq -e --arg uid "$cluster_uid" --argjson generation "$generation" '.metadata.uid == $uid and .metadata.generation == $generation' "$tmp/after.json" >/dev/null || fail 'the database changed during the backup'
read_object objectstores.barmancloud.cnpg.io wedding-db-dedicated "$tmp/store-after.json"
check_store "$tmp/store-after.json"
# This receipt proves the database backup completed. The catalogue catch-up
# separately proves its objects and continuous WAL exist in the dedicated bucket.
jq '{backupCompleted:true,clusterStable:true,backupName:.metadata.name,backupId:.status.backupId,endWal:.status.endWal}' "$tmp/backup.json"
