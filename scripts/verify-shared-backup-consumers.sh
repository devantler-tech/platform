#!/usr/bin/env bash
# Complete #4481's post-removal acceptance with new, identity-bound backups.
# No credentials or backup contents are read into the runner output.
set -euo pipefail
fail() { printf 'shared-backup-consumers: %s\n' "$1" >&2; exit 1; }
[[ "$#" == 0 && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform &&
  "${GITHUB_EVENT_NAME:-}" == workflow_dispatch && "${GITHUB_REF:-}" == refs/heads/main &&
  "${GITHUB_RUN_ATTEMPT:-}" == 1 && "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]*$ &&
  "${WEDDING_SHARED_REMOVAL_CONFIRM:-}" == remove-wedding-shared-backups-4481 ]] ||
  fail 'the confirmed first-attempt main dispatch is required'
umask 077
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
kube() { "${KUBECTL:-kubectl}" --context admin@prod --namespace "$1" --request-timeout=20s "${@:2}"; }
read_object() { kube "$1" get "$2" "$3" -o json >"$4" 2>/dev/null || fail 'a backup prerequisite or receipt could not be read'; }
for namespace in umami coroot; do
  cluster="${namespace}-db"
  name="shared-retirement-${GITHUB_RUN_ID}-${cluster}"
  read_object "${namespace}" clusters.postgresql.cnpg.io "${cluster}" "${work}/cluster.json"
  jq -e --arg store "${cluster}" '
    .metadata.deletionTimestamp == null and (.metadata.uid | type == "string" and length > 0) and
    .spec.instances > 0 and .status.readyInstances == .spec.instances and
    ([.status.conditions[]? | select(.type == "ContinuousArchiving" and .status == "True")] | length == 1) and
    ([.spec.plugins[]? | select(.name == "barman-cloud.cloudnative-pg.io" and .enabled == true and
      .isWALArchiver == true and .parameters.barmanObjectName == $store)] | length == 1)
  ' "${work}/cluster.json" >/dev/null || fail 'a shared database is not healthy and archiving'
  uid="$(jq -r '.metadata.uid' "${work}/cluster.json")"
  read_object "${namespace}" objectstores.barmancloud.cnpg.io "${cluster}" "${work}/store.json"
  jq -e --arg path "s3://platform-backups/cnpg/${cluster}" '
    .metadata.deletionTimestamp == null and .spec.configuration.destinationPath == $path
  ' "${work}/store.json" >/dev/null || fail 'a shared database backup destination changed'
  jq -n --arg name "${name}" --arg ns "${namespace}" --arg cluster "${cluster}" '
    {apiVersion:"postgresql.cnpg.io/v1",kind:"Backup",metadata:{name:$name,namespace:$ns},
      spec:{cluster:{name:$cluster},method:"plugin",pluginConfiguration:{name:"barman-cloud.cloudnative-pg.io"},target:"primary"}}
  ' >"${work}/request.json"
  kube "${namespace}" create -f "${work}/request.json" -o json >"${work}/created.json" 2>/dev/null || fail 'a fresh shared database backup could not be created'
  backup_uid="$(jq -er '.metadata.uid | select(type == "string" and length > 0)' "${work}/created.json")"
  kube "${namespace}" wait "backups.postgresql.cnpg.io/${name}" --for=jsonpath='{.status.phase}'=completed --timeout=20m --request-timeout=21m >/dev/null 2>&1 || fail 'a fresh shared database backup did not complete'
  read_object "${namespace}" backups.postgresql.cnpg.io "${name}" "${work}/backup.json"
  jq -e --arg uid "${backup_uid}" --arg cluster_uid "${uid}" --arg name "${name}" --arg ns "${namespace}" --arg cluster "${cluster}" '
    .metadata.uid == $uid and .metadata.name == $name and .metadata.namespace == $ns and .metadata.deletionTimestamp == null and
    .spec.cluster.name == $cluster and .spec.method == "plugin" and .status.phase == "completed" and
    .status.method == "plugin" and .status.pluginMetadata.clusterUID == $cluster_uid and
    .status.pluginMetadata.name == "barman-cloud.cloudnative-pg.io" and
    (.status.backupId | test("^[0-9]{8}T[0-9]{6}$")) and (.status.endWal | test("^[A-F0-9]{24}$")) and
    (.metadata.creationTimestamp | fromdateiso8601) <= (.status.startedAt | fromdateiso8601) and
    (.status.startedAt | fromdateiso8601) <= (.status.stoppedAt | fromdateiso8601)
  ' "${work}/backup.json" >/dev/null || fail 'a fresh shared database backup receipt is incomplete or changed'
  read_object "${namespace}" clusters.postgresql.cnpg.io "${cluster}" "${work}/cluster-after.json"
  jq -e --slurpfile before "${work}/cluster.json" '
    .metadata.uid == $before[0].metadata.uid and .metadata.generation == $before[0].metadata.generation and
    .spec == $before[0].spec and .metadata.deletionTimestamp == null and .status.readyInstances == .spec.instances
  ' "${work}/cluster-after.json" >/dev/null || fail 'a shared database changed during its backup'
done

read_object velero backupstoragelocations.velero.io default "${work}/location.json"
jq -e '.spec.objectStorage == {bucket:"platform-backups",prefix:"velero/prod"} and .status.phase == "Available"' \
  "${work}/location.json" >/dev/null || fail 'the shared Velero backup location is not available at its reviewed destination'
read_object velero schedules.velero.io daily-full "${work}/schedule.json"
name="shared-retirement-${GITHUB_RUN_ID}-velero"
jq -e --arg name "${name}" '
  .spec.template | select(type == "object" and (.includedNamespaces | type == "array" and length > 0)) |
  {apiVersion:"velero.io/v1",kind:"Backup",metadata:{name:$name,namespace:"velero"},spec:(. + {storageLocation:"default"})}
' "${work}/schedule.json" >"${work}/request.json" || fail 'the normal Velero schedule could not supply a backup request'
kube velero create -f "${work}/request.json" -o json >"${work}/created.json" 2>/dev/null || fail 'a fresh Velero backup could not be created'
backup_uid="$(jq -er '.metadata.uid | select(type == "string" and length > 0)' "${work}/created.json")"
kube velero wait "backups.velero.io/${name}" --for=jsonpath='{.status.phase}'=Completed --timeout=30m --request-timeout=31m >/dev/null 2>&1 || fail 'the fresh Velero backup did not complete'
read_object velero backups.velero.io "${name}" "${work}/backup.json"
jq -e --arg uid "${backup_uid}" --arg name "${name}" '
  .metadata.uid == $uid and .metadata.name == $name and .metadata.namespace == "velero" and .metadata.deletionTimestamp == null and
  .spec.storageLocation == "default" and .status.phase == "Completed" and (.status.errors // 0) == 0 and
  (.metadata.creationTimestamp | fromdateiso8601) <= (.status.startTimestamp | fromdateiso8601) and
  (.status.startTimestamp | fromdateiso8601) <= (.status.completionTimestamp | fromdateiso8601)
' "${work}/backup.json" >/dev/null || fail 'the fresh Velero receipt is incomplete or changed'
printf 'VERIFIED: all three unaffected consumers completed a fresh backup.\n'
