#!/usr/bin/env bash
# Read the original failed-deployment window using CI's existing prod reader.
# Do not publish the privileged reader's inventory or error payloads.
set -euo pipefail
set +x

if [[ "$#" -ne 0 || "${GITHUB_EVENT_NAME:-}" != workflow_dispatch ||
  "${GITHUB_REF:-}" != refs/heads/main ||
  ! "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ||
  "${PRIVATE_MAIL_RECOVERY_CONFIRM:-}" != verify-private-mail-storage-recovery ]]; then
  printf '%s\n' 'MAIL_STORAGE_RECOVERY=UNVERIFIED'
  exit 2
fi

umask 077
proof_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mail-storage-recovery.XXXXXX")"
trap 'rm -rf "$proof_dir"' EXIT
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# The native checker also writes to this file when GitHub supplies it.
unset GITHUB_STEP_SUMMARY
export FLUX_ORPHANS_CONTEXT=admin@prod
export FLUX_ORPHANS_KUBECTL_BIN=kubectl
export FLUX_ORPHANS_SINCE=2026-10-26T07:28:50Z
export FLUX_ORPHANS_BASE_SHA=ac76568f45ac2196f34ea8c4741000ad6db18b84
export FLUX_ORPHANS_RECOVERY_SHA="$GITHUB_SHA"
export FLUX_ORPHANS_SETTLE_SECONDS=60
export FLUX_ORPHANS_RETRY_SECONDS=20
status=0
bash "$root/scripts/check-flux-orphaned-objects.sh" > "$proof_dir/scan.log" 2>&1 || status=$?
case "$status" in
  0)
    if grep -Eq '^✅ (No object is outside every inventory across both reads among |Nothing stayed outside every inventory across (3|4) reads among )' "$proof_dir/scan.log"; then
      printf '%s\n' 'MAIL_STORAGE_RECOVERY=PASS'
      exit 0
    fi
    ;;
  1)
    printf '%s\n' 'MAIL_STORAGE_RECOVERY=FAIL'
    exit 1
    ;;
esac
printf '%s\n' 'MAIL_STORAGE_RECOVERY=UNVERIFIED'
exit 2
