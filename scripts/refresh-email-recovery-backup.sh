#!/usr/bin/env bash
# Run the reviewed daily snapshot template once, without changing cadence or credentials.
set -euo pipefail
umask 077
stage=dispatch
work=''
watchers=()
fail() { printf 'EMAIL_BACKUP_REFRESH=UNVERIFIED stage=%s\n' "$stage" >&2; exit 2; }
cleanup() {
  for pid in "${watchers[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${watchers[@]}"; do wait "$pid" 2>/dev/null || true; done
  if [[ -n "$work" ]]; then rm -rf "$work"; fi
}
trap cleanup EXIT
[[ ${BASH_VERSINFO[0]} -ge 5 && ${GITHUB_EVENT_NAME:-} == workflow_dispatch && ${GITHUB_REF:-} == refs/heads/main && ${GITHUB_REPOSITORY:-} == devantler-tech/platform && ${GITHUB_RUN_ATTEMPT:-} == 1 && ${EMAIL_BACKUP_CONFIRM:-} == refresh-email-recovery-backup && ${GITHUB_RUN_ID:-} =~ ^[1-9][0-9]{0,19}$ ]] || fail
stage=workspace
[[ -d ${EMAIL_BACKUP_WORK_DIR:-} && ! -L "$EMAIL_BACKUP_WORK_DIR" ]] || fail
work=$(mktemp -d "$EMAIL_BACKUP_WORK_DIR/snapshot-refresh.XXXXXXXX") || fail
kc=(kubectl --context admin@prod --namespace openbao --request-timeout=25s)
job="vault-snapshot-refresh-${GITHUB_RUN_ID}-1"
stage=source
"${kc[@]}" get cronjob vault-snapshot -o json > "$work/source.json" 2> "$work/reader.log" || fail
jq -e '
  .kind=="CronJob" and .metadata.name=="vault-snapshot" and .metadata.namespace=="openbao" and
  (.metadata.uid|test("^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$")) and
  (.metadata.resourceVersion|type=="string" and length>0) and
  .metadata.labels["kustomize.toolkit.fluxcd.io/name"]=="infrastructure" and
  .metadata.labels["kustomize.toolkit.fluxcd.io/namespace"]=="flux-system" and
  .spec.suspend!=true and .spec.concurrencyPolicy=="Forbid" and
  ((.status.active//[])|length==0) and
  .spec.jobTemplate.spec.activeDeadlineSeconds==1800 and
  .spec.jobTemplate.spec.backoffLimit==3 and
  (.spec.jobTemplate.spec.template.spec as $p |
    $p.serviceAccountName=="vault-snapshot" and $p.restartPolicy=="OnFailure" and
    $p.automountServiceAccountToken==false and $p.hostNetwork!=true and
    ($p.initContainers|length)==2 and ($p.containers|length)==1 and
    $p.initContainers[0].name=="install-mirror-tools" and
    $p.initContainers[0].image=="docker.io/library/busybox:1.38.0-musl@sha256:ea2b9914a16a4ac1981994af97b318f7c7d4db76b580c56177f08bf76f4a0be8" and
    $p.initContainers[1].name=="snapshot" and
    $p.initContainers[1].image=="quay.io/openbao/openbao:2.5.3@sha256:fdc6da21ca6963560c32336fd7feb9cf2d5e52668f1a1647205a4b41171f0806" and
    $p.containers[0].name=="mirror" and
    $p.containers[0].image=="quay.io/minio/aistor/mc:RELEASE.2026-03-12T04-18-55Z@sha256:6c33dc0fbf65c362be95003cd010ed95a41c556500833ea139f86de40c4c4e9f" and
    ([$p.containers[0].env[]|select(.name=="REQUIRE_REMOTE_SNAPSHOT")|.value]==["true"]) and
    ([$p.volumes[]|select(.persistentVolumeClaim)|{name,claim:.persistentVolumeClaim.claimName}]==[{name:"snapshots",claim:"vault-snapshots-recovery"}]))
' "$work/source.json" > /dev/null 2> "$work/admission.log" || fail
# Construct exactly the observed template and retain its CronJob controller owner.
# Its Forbid policy can therefore see this manual run; history cleanup remains native.
jq --arg name "$job" '
 . as $source |
 {apiVersion:"batch/v1",kind:"Job",metadata:{name:$name,namespace:"openbao",
  labels:((.spec.jobTemplate.metadata.labels//{})+{"platform.devantler.tech/email-backup-refresh":"true"}),
  annotations:((.spec.jobTemplate.metadata.annotations//{})+{"cronjob.kubernetes.io/instantiate":"manual"}),
  ownerReferences:[{apiVersion:"batch/v1",kind:"CronJob",name:$source.metadata.name,uid:$source.metadata.uid,controller:true}]},
  spec:.spec.jobTemplate.spec}
' "$work/source.json" > "$work/job.json" 2> "$work/manifest.log" || fail
"${kc[@]}" get cronjob vault-snapshot -o json > "$work/rebound.json" 2> "$work/reader.log" || fail
jq -e --slurpfile before "$work/source.json" '.metadata.uid==$before[0].metadata.uid and .metadata.resourceVersion==$before[0].metadata.resourceVersion and .spec==$before[0].spec and ((.status.active//[])|length==0)' "$work/rebound.json" > /dev/null 2> "$work/admission.log" || fail
stage=create-unknown
"${kc[@]}" create -f "$work/job.json" -o json > "$work/created.json" 2> "$work/create.log" || fail
jq -e --arg name "$job" --slurpfile source "$work/source.json" '
 .kind=="Job" and .metadata.name==$name and .metadata.namespace=="openbao" and
 (.metadata.uid|test("^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$")) and
 any(.metadata.ownerReferences[]; .kind=="CronJob" and .uid==$source[0].metadata.uid and .controller==true)
' "$work/created.json" > /dev/null 2> "$work/admission.log" || fail
stage=completion
"${kc[@]}" wait --for=condition=Complete "job/$job" --timeout=1850s > "$work/complete.log" 2>&1 &
watchers+=("$!")
"${kc[@]}" wait --for=condition=Failed "job/$job" --timeout=1850s > "$work/failed.log" 2>&1 &
watchers+=("$!")
# A native watch only wakes the readback; it is never itself a success verdict.
wait -n "${watchers[@]}" 2> "$work/wait.log" || true
for pid in "${watchers[@]}"; do kill "$pid" 2>/dev/null || true; done
for pid in "${watchers[@]}"; do wait "$pid" 2>/dev/null || true; done
watchers=()
"${kc[@]}" get job "$job" -o json > "$work/completed.json" 2> "$work/reader.log" || fail
jq -e --slurpfile created "$work/created.json" '
 .metadata.uid==$created[0].metadata.uid and .metadata.name==$created[0].metadata.name and .metadata.namespace=="openbao" and
 .metadata.ownerReferences==$created[0].metadata.ownerReferences and .status.succeeded==1 and
 any(.status.conditions[]; .type=="Complete" and .status=="True") and
 all(.status.conditions[]; .type!="Failed" or .status!="True")
' "$work/completed.json" > /dev/null 2> "$work/admission.log" || fail
stage=pod-proof
uid=$(jq -er '.metadata.uid' "$work/created.json") || fail
"${kc[@]}" get pods -l "batch.kubernetes.io/controller-uid=$uid" -o json > "$work/pods.json" 2> "$work/reader.log" || fail
jq -e --arg uid "$uid" '
 [.items[]|select(any(.metadata.ownerReferences[]?;.kind=="Job" and .uid==$uid and .controller==true))|select(.status.phase=="Succeeded")]
 | length==1 and all(.[];
   ([.status.initContainerStatuses[]|select(.state.terminated.exitCode==0)|.name]|sort)==["install-mirror-tools","snapshot"] and
   ([.status.containerStatuses[]|select(.state.terminated.exitCode==0)|.name]==["mirror"]))
' "$work/pods.json" > /dev/null 2> "$work/admission.log" || fail
printf '%s\n' 'EMAIL_BACKUP_REFRESH=COMPLETED snapshot=1 mirror=1 restore=UNVERIFIED'
