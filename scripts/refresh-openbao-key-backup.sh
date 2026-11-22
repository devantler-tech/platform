#!/usr/bin/env bash
# Seed the first independent recovery-key archive if its daily schedule has no backup yet.
set +x
set -euo pipefail
umask 077
stage=dispatch
fail() {
  printf 'OPENBAO_KEY_BACKUP=UNVERIFIED stage=%s\n' "$stage" >&2
  exit 2
}

[[ ${GITHUB_EVENT_NAME:-} == workflow_dispatch && ${GITHUB_REF:-} == refs/heads/main &&
  ${GITHUB_REPOSITORY:-} == devantler-tech/platform && ${GITHUB_RUN_ATTEMPT:-} == 1 &&
  ${RUNNER_ENVIRONMENT:-} == github-hosted && ${OPENBAO_BACKUP_CONFIRM:-} == bootstrap-openbao-key-backup &&
  ${GITHUB_SHA:-} =~ ^[0-9a-f]{40}$ ]] || fail
stage=workspace
work=${1:-}
[[ $work == /dev/shm/openbao-recovery.* && -d $work && ! -L $work && "${KUBECONFIG:-}" == "$work/kubeconfig" ]] || fail
readonly schedule=velero-openbao-recovery-keys
readonly name="${schedule}-bootstrap"
kc=(kubectl --context admin@prod -n velero --request-timeout=25s)
stage=schedule
"${kc[@]}" get schedules.velero.io "$schedule" -o json >"$work/key-schedule.json" 2>"$work/key-reader.log" || fail
jq -e --arg schedule "$schedule" --arg name "$name" '
  def backup_spec: {
    ttl:"336h0m0s", storageLocation:"default", includedNamespaces:["openbao"], includedResources:["secrets"],
    includeClusterResources:false, snapshotVolumes:false, snapshotMoveData:false, defaultVolumesToFsBackup:false
  };
  select(.apiVersion=="velero.io/v1" and .kind=="Schedule" and .metadata.name==$schedule and
    .metadata.namespace=="velero" and .metadata.deletionTimestamp==null and
    (.metadata.uid | type=="string" and test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")) and
    (.metadata.resourceVersion | type=="string" and length>0) and
    .status.phase=="Enabled" and (.spec.paused//false)==false and (.spec.useOwnerReferencesInBackup//false)==false and
    .spec.schedule=="10 6 * * *" and
    (.spec.template | if .ttl=="336h" then .ttl="336h0m0s" else . end)==backup_spec) |
  {apiVersion:"velero.io/v1", kind:"Backup",
   metadata:{name:$name, namespace:"velero", labels:{"velero.io/schedule-name":$schedule}},
   spec:backup_spec}
' "$work/key-schedule.json" >"$work/key-backup.json" 2>"$work/key-validation.log" || fail
stage=storage-location
"${kc[@]}" get backupstoragelocations.velero.io default -o json >"$work/key-location.json" 2>"$work/key-reader.log" || fail
jq -e '.apiVersion=="velero.io/v1" and .kind=="BackupStorageLocation" and .metadata.name=="default" and
  .metadata.namespace=="velero" and .metadata.deletionTimestamp==null and .status.phase=="Available" and
  .spec.accessMode=="ReadWrite" and .spec.provider=="aws"' "$work/key-location.json" >/dev/null 2>"$work/key-validation.log" || fail
stage=existing-backups
"${kc[@]}" get backups.velero.io -l "velero.io/schedule-name=$schedule" --chunk-size=0 -o json >"$work/key-backups.json" 2>"$work/key-reader.log" || fail
jq -e '((.apiVersion=="v1" and .kind=="List") or (.apiVersion=="velero.io/v1" and .kind=="BackupList")) and
  (.metadata.continue//"")=="" and (.items | type)=="array"' "$work/key-backups.json" >/dev/null 2>"$work/key-validation.log" || fail
if jq -e --arg schedule "$schedule" --slurpfile plan "$work/key-backup.json" '
  any(.items[]?;
    .apiVersion=="velero.io/v1" and .kind=="Backup" and .metadata.namespace=="velero" and
    .metadata.labels["velero.io/schedule-name"]==$schedule and .spec.includeClusterResources==false and
    .metadata.deletionTimestamp==null and .spec.includedNamespaces==$plan[0].spec.includedNamespaces and
    .spec.includedResources==$plan[0].spec.includedResources and .spec.storageLocation==$plan[0].spec.storageLocation and
    .status.phase=="Completed" and (.status.errors//0)==0)
' "$work/key-backups.json" >/dev/null 2>"$work/key-validation.log"; then
  printf '%s\n' 'OPENBAO_KEY_BACKUP=EXISTS restore=UNVERIFIED'
  exit 0
fi
jq -e '.items==[]' "$work/key-backups.json" >/dev/null 2>"$work/key-validation.log" || fail
stage=current-source
latest=$(git ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main) || fail
[[ $latest == "$GITHUB_SHA"$'\trefs/heads/main' ]] || fail
stage='schedule-rebind'
"${kc[@]}" get schedules.velero.io "$schedule" -o json >"$work/key-rebound.json" 2>"$work/key-reader.log" || fail
jq -e --slurpfile prior "$work/key-schedule.json" '
  .metadata.uid==$prior[0].metadata.uid and .metadata.resourceVersion==$prior[0].metadata.resourceVersion and
  .metadata.deletionTimestamp==null and .spec==$prior[0].spec
' "$work/key-rebound.json" >/dev/null 2>"$work/key-validation.log" || fail
stage=create-unknown
"${kc[@]}" create -f "$work/key-backup.json" -o json >"$work/key-created.json" 2>"$work/key-create.log" || fail
stage=completion
"${kc[@]}" --request-timeout=320s wait --for='jsonpath={.status.phase}=Completed' "backups.velero.io/$name" --timeout=5m >"$work/key-wait.log" 2>&1 || fail
"${kc[@]}" get backups.velero.io "$name" -o json >"$work/key-completed.json" 2>"$work/key-reader.log" || fail
jq -e --arg name "$name" --arg schedule "$schedule" --slurpfile created "$work/key-created.json" '
  .apiVersion=="velero.io/v1" and .kind=="Backup" and .metadata.name==$name and .metadata.namespace=="velero" and
  .metadata.labels["velero.io/schedule-name"]==$schedule and .metadata.deletionTimestamp==null and
  (.metadata.uid | type=="string" and test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")) and
  .metadata.uid==$created[0].metadata.uid and .spec.includedNamespaces==["openbao"] and .spec.includedResources==["secrets"] and
  .spec.storageLocation=="default" and .spec.includeClusterResources==false and (.spec.snapshotVolumes//false)==false and
  (.spec.snapshotMoveData//false)==false and (.spec.defaultVolumesToFsBackup//false)==false and
  .status.phase=="Completed" and (.status.errors//0)==0
' "$work/key-completed.json" >/dev/null 2>"$work/key-validation.log" || fail
printf '%s\n' 'OPENBAO_KEY_BACKUP=COMPLETED restore=UNVERIFIED'
