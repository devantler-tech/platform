#!/usr/bin/env bash
# Restore only the fixed input record; never stop readers or authorize data erasure.
set +x
set -euo pipefail
umask 077
ulimit -c 0
stage=dispatch
native_dir=
proof_dir=
forward_pid=
verified=false

cleanup() {
  local rc=$?
  trap - EXIT
  if [[ -n "$forward_pid" ]]; then
    if ! kill "$forward_pid" 2>/dev/null; then
      rc=2
      stage=connection-cleanup
    fi
    wait "$forward_pid" 2>/dev/null || true
  fi
  if ! rm -rf -- "$native_dir" "$proof_dir" 2>/dev/null; then
    rc=2
    stage=private-cleanup
  fi
  if [[ $rc == 0 && $verified == true ]]; then
    printf '%s\n' 'EMAIL_RETAINED_INPUT_RECOVERY=VERIFIED source_writes=0 mailbox_data=UNVERIFIED key=UNVERIFIED restart=NOT_AUTHORIZED'
  else
    printf 'EMAIL_RETAINED_INPUT_RECOVERY=UNVERIFIED stage=%s restart=NOT_AUTHORIZED\n' "$stage" >&2
    [[ $rc != 0 ]] || rc=2
  fi
  exit "$rc"
}
trap cleanup EXIT
fail() { exit 2; }
[[ $# == 0 && ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == workflow_dispatch && ${GITHUB_REF:-} == refs/heads/main && ${GITHUB_REPOSITORY:-} == devantler-tech/platform && ${GITHUB_ACTOR:-} == devantler && ${GITHUB_RUN_ATTEMPT:-} == 1 && ${RUNNER_ENVIRONMENT:-} == github-hosted && ${GITHUB_WORKFLOW_REF:-} == devantler-tech/platform/.github/workflows/verify-retained-email-reader-inputs.yaml@refs/heads/main && ${EMAIL_READER_INPUTS_CONFIRM:-} == verify-retained-email-reader-inputs ]] || fail
stage=workspace
[[ $(id -u) != 0 && $(findmnt -n -o FSTYPE --target /dev/shm) == tmpfs ]] || fail
native_dir=$(mktemp -d /dev/shm/email-input-native.XXXXXXXX)
proof_dir=$(mktemp -d /dev/shm/email-input-proof.XXXXXXXX)
binary="$RUNNER_TEMP/email-input-recovery-tools/recovery"
reader="$RUNNER_TEMP/email-input-recovery-tools/reader"
stage=current-source
if ! ./scripts/verify-prod-recovery-source.sh "$GITHUB_SHA" >/dev/null 2>"$native_dir/source.log"; then fail; fi
stage=private-inputs
[[ -n ${KUBE_CONFIG:-} && ${#KUBE_CONFIG} -le 1048576 ]] || fail
export KUBECONFIG="$native_dir/kubeconfig"
printf '%s' "$KUBE_CONFIG" >"$KUBECONFIG"
unset KUBE_CONFIG
stage=stable-api
# Preserve the helper's masking stream, never its private endpoint stdout.
if ! ./scripts/use-prod-stable-api-endpoint.sh >/dev/null; then fail; fi
unset HCLOUD_TOKEN
kc=(kubectl --context=admin@prod --request-timeout=20s)
stage=read-only-credential
if ! "${kc[@]}" -n openbao get secret openbao-unseal -o jsonpath='{.data.root-token}' 2>"$native_dir/token-read.log" |
  base64 --decode >"$native_dir/token" 2>"$native_dir/decode.log"; then fail; fi
[[ -s "$native_dir/token" ]] || fail

open_forward() {
  "${kc[@]}" -n openbao port-forward --address=127.0.0.1 service/openbao-active 38200:8200 >"$native_dir/forward.log" 2>&1 &
  forward_pid=$!
  local ready=false attempt
  for ((attempt = 0; attempt < 20; attempt++)); do
    kill -0 "$forward_pid" 2>/dev/null || fail
    if curl --disable --silent --fail --noproxy '*' --connect-timeout 1 --max-time 2 http://127.0.0.1:38200/v1/sys/health >"$native_dir/health.json" 2>"$native_dir/health.log" &&
      jq -e '.initialized==true and .sealed==false and .standby==false' "$native_dir/health.json" >/dev/null; then
      ready=true
      break
    fi
    sleep 1
  done
  [[ "$ready" == true ]] || fail
}
close_forward() {
  kill "$forward_pid" 2>/dev/null || fail
  wait "$forward_pid" 2>/dev/null || true
  forward_pid=
}
native_parity() {
  if ! "$reader" --verify-reader-inputs >"$native_dir/parity-result" 2>"$native_dir/parity.log"; then fail; fi
  [[ $(cat "$native_dir/parity-result") == 'EMAIL_READER_INPUT_PARITY=VERIFIED source_writes=0 restart=NOT_AUTHORIZED recovery=UNVERIFIED' ]] || fail
}

stage=pre-restore-parity
open_forward
export EMAIL_READER_INPUTS_WORK_DIR="$native_dir"
native_parity
stage=collection
export OPENBAO_RECOVERY_DIR="$proof_dir"
if ! "$binary" collect-email-retained-inputs >"$native_dir/collection-result" 2>"$native_dir/collection.log"; then fail; fi
[[ $(cat "$native_dir/collection-result") == OPENBAO_BACKUP_INPUTS=PASS ]] || fail
stage=private-handoff
close_forward
unset KUBECONFIG EMAIL_READER_INPUTS_WORK_DIR OPENBAO_RECOVERY_DIR
# The live token/kubeconfig remain in a separate, unmounted RAM directory.
# Only independently archived recovery keys and complete expectations enter /proof.
stage=image
image=$("$binary" image) || fail
if ! docker pull "$image" >"$native_dir/pull.log" 2>&1; then fail; fi
stage=isolated-restore
if ! bash scripts/run-openbao-backup-sandbox.sh verify-email-retained-inputs "$proof_dir" "$binary" >"$native_dir/restore-result" 2>"$native_dir/restore.log"; then fail; fi
[[ $(cat "$native_dir/restore-result") == 'OPENBAO_BACKUP_RESTORE=PASS records=1 version=2.6.3' ]] || fail
stage=post-restore-source
if ! ./scripts/verify-prod-recovery-source.sh "$GITHUB_SHA" >/dev/null 2>"$native_dir/source.log"; then fail; fi
stage=post-restore-parity
export KUBECONFIG="$native_dir/kubeconfig"
export EMAIL_READER_INPUTS_WORK_DIR="$native_dir"
open_forward
native_parity
stage=connection-cleanup
close_forward
verified=true
