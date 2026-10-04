#!/usr/bin/env bash
# Exercise the production backup request and its refusal boundaries without a cluster.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
mkdir "$tmp/bin"
cat >"$tmp/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2 $3 $4 $5" == '--context admin@prod --namespace wedding-app --request-timeout=20s' ]] || exit 9
shift 5
printf '%s\n' "$*" >>"$CASE/calls"
case "$1 $2" in
  'get clusters.postgresql.cnpg.io')
    if [[ -e "$CASE/cluster-read" ]]; then cat "$CASE/cluster-after.json";
    else touch "$CASE/cluster-read"; cat "$CASE/cluster.json"; fi ;;
  'get objectstores.barmancloud.cnpg.io') cat "$CASE/store.json" ;;
  'create -f') cp "$3" "$CASE/request.json"; cat "$CASE/created.json" ;;
  'get backups.postgresql.cnpg.io') cat "$CASE/backup.json" ;;
  *) exit 9 ;;
esac
SH
chmod +x "$tmp/bin/kubectl"
new_case() {
  CASE="$tmp/$1"; export CASE; mkdir "$CASE"
  cat >"$CASE/cluster.json" <<'JSON'
{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","metadata":{"name":"wedding-db","namespace":"wedding-app","uid":"00000000-0000-0000-0000-000000000001","generation":7},"spec":{"instances":3,"plugins":[{"name":"barman-cloud.cloudnative-pg.io","enabled":true,"isWALArchiver":true,"parameters":{"barmanObjectName":"wedding-db-dedicated","serverName":"wedding-db-20260909"}}]},"status":{"readyInstances":3,"conditions":[{"type":"Ready","status":"True"},{"type":"ContinuousArchiving","status":"True"}]}}
JSON
  cp "$CASE/cluster.json" "$CASE/cluster-after.json"
  cat >"$CASE/store.json" <<'JSON'
{"apiVersion":"barmancloud.cnpg.io/v1","kind":"ObjectStore","metadata":{"name":"wedding-db-dedicated","namespace":"wedding-app"},"spec":{"configuration":{"destinationPath":"s3://wedding-db-backups/cnpg/wedding-db","s3Credentials":{"accessKeyId":{"name":"wedding-db-backup-r2-dedicated","key":"ACCESS_KEY_ID"},"secretAccessKey":{"name":"wedding-db-backup-r2-dedicated","key":"SECRET_ACCESS_KEY"},"region":{"name":"wedding-db-backup-r2-dedicated","key":"REGION"}}}}}
JSON
  cat >"$CASE/created.json" <<'JSON'
{"apiVersion":"postgresql.cnpg.io/v1","kind":"Backup","metadata":{"name":"wedding-db-dedicated-proof-12345","namespace":"wedding-app","uid":"00000000-0000-0000-0000-000000000002","creationTimestamp":"2026-10-01T06:00:00Z"},"spec":{"cluster":{"name":"wedding-db"},"method":"plugin","pluginConfiguration":{"name":"barman-cloud.cloudnative-pg.io"},"target":"primary"}}
JSON
  jq '.+{status:{phase:"completed",method:"plugin",backupId:"20261001T060001",startedAt:"2026-10-01T06:00:02Z",stoppedAt:"2026-10-01T06:00:12Z",endWal:"000000300000000000000087",pluginMetadata:{clusterUID:"00000000-0000-0000-0000-000000000001",name:"barman-cloud.cloudnative-pg.io"}}}' "$CASE/created.json" >"$CASE/backup.json"
}
run_case() {
  rc=0
  env PATH="$tmp/bin:$PATH" GITHUB_REPOSITORY=devantler-tech/platform \
    GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF=refs/heads/main \
    GITHUB_RUN_ID=12345 GITHUB_RUN_ATTEMPT=1 \
    WEDDING_BACKUP_CONFIRM=verify-wedding-backup-cutover \
    "$@" bash "$root/scripts/verify-wedding-backup-cutover.sh" \
    >"$CASE/out" 2>"$CASE/err" || rc=$?
}
refused_without_backup() {
  [[ "$rc" != 0 ]] || fail "$1 was accepted"
  [[ ! -e "$CASE/request.json" ]] || fail "$1 created a backup before refusal"
}
new_case completed
run_case
[[ "$rc" == 0 ]] || fail "a completed dedicated backup must pass (exit $rc)"
jq -e '.spec=={cluster:{name:"wedding-db"},method:"plugin",pluginConfiguration:{name:"barman-cloud.cloudnative-pg.io"},target:"primary"}' "$CASE/request.json" >/dev/null || fail 'wrong production backup request'
jq -e '.backupCompleted==true and .clusterStable==true and .backupId=="20261001T060001" and .endWal=="000000300000000000000087"' "$CASE/out" >/dev/null || fail 'completion receipt is missing'
if rg -q 'secrets|patch|delete|exec' "$CASE/calls"; then fail 'proof exceeded its API boundary'; fi
for mutation in shared-archive unhealthy missing-replica duplicate-plugin archiving-failed wrong-server deleting wrong-destination reused-credential; do
  new_case "$mutation"
  case "$mutation" in
    shared-archive) filter='.spec.plugins[0].parameters.barmanObjectName="wedding-db"' ;;
    unhealthy) filter='.status.readyInstances=0' ;;
    missing-replica) filter='.status.readyInstances=2' ;;
    duplicate-plugin) filter='.spec.plugins += [.spec.plugins[0] | .parameters.barmanObjectName="wedding-db"]' ;;
    archiving-failed) filter='.status.conditions[1].status="False"' ;;
    wrong-server) filter='.spec.plugins[0].parameters.serverName="wedding-db"' ;;
    deleting) filter='.metadata.deletionTimestamp="2026-10-01T05:59:59Z"' ;;
    wrong-destination) filter='.spec.configuration.destinationPath="s3://platform-backups/cnpg/wedding-db"' ;;
    reused-credential) filter='.spec.configuration.s3Credentials.accessKeyId.name="wedding-db-backup-r2"' ;;
  esac
  file=cluster; [[ "$mutation" != wrong-destination && "$mutation" != reused-credential ]] || file=store
  jq "$filter" "$CASE/$file.json" >"$CASE/changed.json"; mv "$CASE/changed.json" "$CASE/$file.json"
  run_case; refused_without_backup "$mutation"
done
for field in GITHUB_REF GITHUB_EVENT_NAME GITHUB_RUN_ATTEMPT GITHUB_RUN_ID GITHUB_REPOSITORY WEDDING_BACKUP_CONFIRM; do
  new_case "wrong-$field"
  run_case "$field=wrong"
  refused_without_backup "$field"
  [[ ! -e "$CASE/calls" ]] || fail "$field reached the API"
done
for mutation in failed unknown-phase wrong-backup-identity stale-backup wrong-backup-cluster different-database shared-after-create; do
  new_case "$mutation"
  file=backup
  case "$mutation" in
    failed) filter='.status.phase="failed"' ;;
    unknown-phase) filter='.status.phase="unknown"' ;;
    wrong-backup-identity) filter='.metadata.uid="00000000-0000-0000-0000-000000000099"' ;;
    stale-backup) filter='.status.startedAt="2026-09-30T06:00:02Z"' ;;
    wrong-backup-cluster) filter='.status.pluginMetadata.clusterUID="00000000-0000-0000-0000-000000000099"' ;;
    different-database) file=cluster-after; filter='.metadata.uid="00000000-0000-0000-0000-000000000099"' ;;
    shared-after-create) file=cluster-after; filter='.spec.plugins[0].parameters.barmanObjectName="wedding-db"' ;;
  esac
  jq "$filter" "$CASE/$file.json" >"$CASE/changed.json"; mv "$CASE/changed.json" "$CASE/$file.json"
  run_case
  [[ "$rc" != 0 ]] || fail "$mutation was reported as verified"
done
printf 'PASS: completed dedicated backup and 22 refusal controls\n'
