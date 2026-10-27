#!/usr/bin/env bash
# Run only the reviewed fixed-endpoint verifier in a disposable, isolated server.
set -euo pipefail
umask 077

fail() { printf '%s\n' 'OPENBAO_BACKUP_RESTORE=FAIL stage=runtime' >&2; exit 2; }
[[ $# == 3 && $(uname -s) == Linux && $(id -u) != 0 ]] || fail
mode=$1
proof_dir=$2
binary=$3
[[ $mode == verify || $mode == fixture ]] || fail
[[ $proof_dir == /* && -d $proof_dir && ! -L $proof_dir && $binary == /* && -f $binary && ! -L $binary && -x $binary ]] || fail
[[ $(cd "$proof_dir" && pwd -P) == "$proof_dir" && $(stat -c %a "$proof_dir") == 700 && $(stat -c %u "$proof_dir") == "$(id -u)" ]] || fail
image=$("$binary" image) || fail
[[ $image =~ ^quay\.io/openbao/openbao:2\.6\.3@sha256:[a-f0-9]{64}$ ]] || fail
nonce=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
[[ $nonce =~ ^[a-f0-9]{32}$ ]] || fail
name="openbao-recovery-$nonce"

cleanup() {
  local rc=$? ids label
  trap - EXIT
  if ! ids=$(docker container ls --all --no-trunc --filter "name=^/$name$" --format '{{.ID}}' 2>"$proof_dir/cleanup.log"); then
    printf '%s\n' 'OPENBAO_BACKUP_CLEANUP=UNVERIFIED' >&2
    [[ $rc != 0 ]] || rc=2
  elif [[ -n $ids ]]; then
    if [[ $ids =~ ^[a-f0-9]{64}$ ]] &&
        label=$(docker inspect --type container --format '{{ index .Config.Labels "platform.devantler.tech/recovery-proof" }}' "$ids" 2>>"$proof_dir/cleanup.log") &&
        [[ $label == "$nonce" ]] &&
        docker rm --force "$ids" >"$proof_dir/cleanup.log" 2>&1; then
      :
    else
      printf '%s\n' 'OPENBAO_BACKUP_CLEANUP=UNVERIFIED' >&2
      [[ $rc != 0 ]] || rc=2
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT

arguments=(run --rm --name "$name" --label "platform.devantler.tech/recovery-proof=$nonce"
  --network=none --read-only --cap-drop=ALL --security-opt=no-new-privileges
  --memory=2g --cpus=1 --pids-limit=128 --user "$(id -u):$(id -g)"
  --tmpfs "/work:rw,nosuid,nodev,noexec,size=768m,mode=0700,uid=$(id -u),gid=$(id -g)"
  --tmpfs "/openbao/audit:rw,nosuid,nodev,noexec,size=64m,mode=0700,uid=$(id -u),gid=$(id -g)"
  # Shadow the image's declared VOLUMEs; never create disk-backed anonymous
  # volumes for a production recovery proof.
  --tmpfs "/openbao/file:ro,nosuid,nodev,noexec,size=1m,mode=0700,uid=$(id -u),gid=$(id -g)"
  --tmpfs "/openbao/logs:ro,nosuid,nodev,noexec,size=1m,mode=0700,uid=$(id -u),gid=$(id -g)"
  --mount "type=bind,source=$binary,target=/verifier,readonly"
  --entrypoint /verifier)
if [[ $mode == fixture ]]; then
  arguments+=(--env OPENBAO_RECOVERY_SYNTHETIC=1 --mount "type=bind,source=$proof_dir,target=/export")
else
  arguments+=(--mount "type=bind,source=$proof_dir,target=/proof,readonly")
fi

rc=0
docker "${arguments[@]}" "$image" "$mode" >"$proof_dir/sandbox-result" 2>"$proof_dir/sandbox-error.log" || rc=$?
result=$(cat "$proof_dir/sandbox-result")
if [[ $rc == 0 && $mode == fixture && $result == OPENBAO_BACKUP_FIXTURE=PASS ]]; then
  printf '%s\n' "$result"
elif [[ $rc == 0 && $mode == verify && $result == 'OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3' ]]; then
  printf '%s\n' "$result"
else
  # All container and API diagnostics remain private. Only a fixed helper
  # refusal vocabulary is permitted in public output.
  if [[ $result =~ ^OPENBAO_BACKUP_RESTORE=FAIL\ stage=(mode|inputs|snapshot|restore|unseal|records|membership|restored_state|seal|sandbox|server|initialization|verification)$ ]]; then
    printf '%s\n' "$result" >&2
  else
    printf '%s\n' 'OPENBAO_BACKUP_RESTORE=FAIL stage=runtime' >&2
  fi
  exit 2
fi
