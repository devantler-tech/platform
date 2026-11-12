#!/usr/bin/env bash
# Preserve a separate immutable restart witness; never stop readers or erase data.
set +x
set -euo pipefail
umask 077
ulimit -c 0
stage=dispatch
native_dir=
proof_dir=
forward_pid=
verified=false
fingerprint=

cleanup() {
  local rc=$?
  trap - EXIT
  if [[ -n "$forward_pid" ]]; then
    if ! kill "$forward_pid" 2>/dev/null; then rc=2; stage=connection-cleanup; fi
    wait "$forward_pid" 2>/dev/null || true
  fi
  if ! rm -rf -- "$native_dir" "$proof_dir" 2>/dev/null; then rc=2; stage=private-cleanup; fi
  if [[ $rc == 0 && $verified == true && $fingerprint =~ ^[0-9a-f]{64}$ ]]; then
    printf 'EMAIL_READER_RESTART_WITNESS=STORED record_sha256=%s source_writes=0 key=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED\n' "$fingerprint"
  else
    printf 'EMAIL_READER_RESTART_WITNESS=UNVERIFIED stage=%s restart=NOT_AUTHORIZED recovery=UNVERIFIED\n' "$stage" >&2
    [[ $rc != 0 ]] || rc=2
  fi
  exit "$rc"
}
trap cleanup EXIT
fail() { exit 2; }
[[ $# == 0 && ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == workflow_dispatch && ${GITHUB_REF:-} == refs/heads/main && ${GITHUB_REPOSITORY:-} == devantler-tech/platform && ${GITHUB_ACTOR:-} == devantler && ${GITHUB_RUN_ATTEMPT:-} == 1 && ${RUNNER_ENVIRONMENT:-} == github-hosted && ${GITHUB_WORKFLOW_REF:-} == devantler-tech/platform/.github/workflows/prepare-retained-email-reader-witness.yaml@refs/heads/main && ${EMAIL_READER_WITNESS_CONFIRM:-} == prepare-retained-email-reader-witness ]] || fail
stage=independent-witnesses
replica_witnesses=${EMAIL_RETAINED_REPLICA_UIDS:-}
[[ -n "$replica_witnesses" && ${#replica_witnesses} -le 256 ]] || fail
if ! jq -en --arg r "${EMAIL_RETAINED_READER_UID:-}" --arg c "${EMAIL_RETAINED_CLAIM_UID:-}" --arg p "${EMAIL_RETAINED_PV_UID:-}" --arg b "${EMAIL_RETAINED_BACKEND_UID:-}" --argjson replicas "$replica_witnesses" '
  $replicas | type=="array" and length==3 and
  ([$r,$c,$p,$b]+$replicas | length==7 and (unique|length)==7 and all(type=="string" and test("^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")))
' >/dev/null 2>&1; then fail; fi
stage=workspace
[[ $(id -u) != 0 && $(findmnt -n -o FSTYPE --target /dev/shm) == tmpfs ]] || fail
native_dir=$(mktemp -d /dev/shm/email-input-native.XXXXXXXX)
proof_dir=$(mktemp -d /dev/shm/email-input-proof.XXXXXXXX)
binary="$RUNNER_TEMP/email-restart-witness-tools/recovery"
reader="$RUNNER_TEMP/email-restart-witness-tools/reader"
stage=current-source
if ! ./scripts/verify-prod-recovery-source.sh "$GITHUB_SHA" >/dev/null 2>"$native_dir/source.log"; then fail; fi
stage=private-inputs
[[ -n ${KUBE_CONFIG:-} && ${#KUBE_CONFIG} -le 1048576 ]] || fail
export KUBECONFIG="$native_dir/kubeconfig"
printf '%s' "$KUBE_CONFIG" >"$KUBECONFIG"
unset KUBE_CONFIG
stage=stable-api
if ! ./scripts/use-prod-stable-api-endpoint.sh >/dev/null; then fail; fi
unset HCLOUD_TOKEN
kc=(kubectl --context=admin@prod --request-timeout=20s)

open_forward() {
  "${kc[@]}" -n openbao port-forward --address=127.0.0.1 service/openbao-active 38200:8200 >"$native_dir/forward.log" 2>&1 &
  forward_pid=$!
  local ready=false attempt
  for ((attempt = 0; attempt < 20; attempt++)); do
    kill -0 "$forward_pid" 2>/dev/null || fail
    if curl --disable --silent --fail --noproxy '*' --connect-timeout 1 --max-time 2 http://127.0.0.1:38200/v1/sys/health >"$native_dir/health.json" 2>"$native_dir/health.log" &&
      jq -e '.initialized==true and .sealed==false and .standby==false' "$native_dir/health.json" >/dev/null; then ready=true; break; fi
    sleep 1
  done
  [[ "$ready" == true ]] || fail
}
close_forward() {
  kill "$forward_pid" 2>/dev/null || fail
  wait "$forward_pid" 2>/dev/null || true
  forward_pid=
}

stage=collection
open_forward
export OPENBAO_RECOVERY_DIR="$proof_dir"
if ! "$binary" collect-email-retained-inputs >"$native_dir/collection-result" 2>"$native_dir/collection.log"; then fail; fi
[[ $(cat "$native_dir/collection-result") == OPENBAO_BACKUP_INPUTS=PASS ]] || fail
cp "$proof_dir/root-token" "$native_dir/token"
cp "$proof_dir/expected.json" "$native_dir/expected.json"
export EMAIL_READER_INPUTS_WORK_DIR="$native_dir"
stage=native-preflight
if ! "$reader" --inspect-reader-witness >"$native_dir/preflight-result" 2>"$native_dir/preflight.log"; then fail; fi
[[ $(cat "$native_dir/preflight-result") == 'EMAIL_READER_RESTART_WITNESS=PREFLIGHT source_writes=0 key=UNVERIFIED restart=NOT_AUTHORIZED recovery=UNVERIFIED' ]] || fail
stage=private-handoff
close_forward
unset KUBECONFIG EMAIL_READER_INPUTS_WORK_DIR OPENBAO_RECOVERY_DIR
stage=image
image=$("$binary" image) || fail
if ! docker pull "$image" >"$native_dir/pull.log" 2>&1; then fail; fi
stage=isolated-restore
if ! bash scripts/run-openbao-backup-sandbox.sh verify-email-retained-inputs "$proof_dir" "$binary" >"$native_dir/restore-result" 2>"$native_dir/restore.log"; then fail; fi
[[ $(cat "$native_dir/restore-result") == 'OPENBAO_BACKUP_RESTORE=PASS records=1 version=2.6.3' ]] || fail
stage=post-restore-source
if ! ./scripts/verify-prod-recovery-source.sh "$GITHUB_SHA" >/dev/null 2>"$native_dir/source.log"; then fail; fi
export KUBECONFIG="$native_dir/kubeconfig"
export EMAIL_READER_INPUTS_WORK_DIR="$native_dir"
open_forward
stage=witness-custody
if ! "$reader" --prepare-reader-witness >"$native_dir/witness-result" 2>"$native_dir/witness.log"; then fail; fi
result=$(cat "$native_dir/witness-result")
if [[ $result =~ ^EMAIL_READER_RESTART_WITNESS=STORED\ record_sha256=([0-9a-f]{64})\ source_writes=0\ key=UNVERIFIED\ restart=NOT_AUTHORIZED\ recovery=UNVERIFIED$ ]]; then fingerprint=${BASH_REMATCH[1]}; else fail; fi
stage=connection-cleanup
close_forward
verified=true
