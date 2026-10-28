#!/usr/bin/env bash
# Read fixed metadata only. Secret/key/mail contents and environment values are
# never emitted. The outside observer accepts only this complete bounded schema.
set -euo pipefail

# Represent an effective access check without opening or changing its target.
flag() { if "$@" >/dev/null 2>&1; then printf true; else printf false; fi; }

# Require a complete small kernel file. A sentinel preserves trailing newlines
# while measuring, so a truncated record cannot masquerade as a shorter read.
bounded_rows() {
  local task_rows
  task_rows="$(head -c 1048577 "$1" || exit 3; printf '.')" || exit 3
  task_rows="${task_rows%.}"
  [[ "${#task_rows}" -le 1048576 ]] || exit 3
  printf '%s' "$task_rows"
}

IFS= read -r task_comm </proc/1/comm
task_pid=false
[[ "$task_comm" != stalwart ]] || task_pid=true
task_uid="$(id -u)"
task_uid_ok=false
[[ "$task_uid" != 2000 ]] || task_uid_ok=true
task_hash="$(sha256sum /etc/stalwart/config.json)"
task_hash="${task_hash%% *}"
[[ "$task_hash" =~ ^[0-9a-f]{64}$ ]]
task_cert="$(flag test -r /etc/stalwart/tls/tls.crt)"
task_key="$(flag test -r /etc/stalwart/tls/tls.key)"
task_data_read=false
[[ ! -r /var/lib/stalwart/data || ! -x /var/lib/stalwart/data ]] || task_data_read=true
task_data_write="$(flag test -w /var/lib/stalwart/data)"
task_tmp_write="$(flag test -w /tmp)"
[[ ! -L /var/lib/stalwart/data/LOCK ]]
task_lock="$(stat -Lc '%d %i' /var/lib/stalwart/data/LOCK)"
read -r task_device task_inode <<<"$task_lock"
[[ "$task_device" =~ ^[0-9]+$ && "$task_inode" =~ ^[0-9]+$ ]]
[[ -r /proc/locks ]]
task_locks=()
task_lock_rows="$(bounded_rows /proc/locks)"
while read -r _task_id task_type task_advisory task_access task_owner task_identity task_start task_end task_extra; do
  if [[ "$task_type" == POSIX && "$task_advisory" == ADVISORY && "$task_access" == WRITE && "$task_owner" == 1 && "$task_start" == 0 && "$task_end" == EOF && -z "$task_extra" ]]; then
    [[ "$task_identity" =~ ^[0-9a-f]+:[0-9a-f]+:[0-9]+$ ]]
    task_locks+=("$task_identity")
  fi
done <<<"$task_lock_rows"
[[ "$task_lock" == "$(stat -Lc '%d %i' /var/lib/stalwart/data/LOCK)" ]]
task_management=0
task_recovery=0
for task_table in /proc/net/tcp /proc/net/tcp6; do
  [[ -r "$task_table" ]]
  task_tcp_rows="$(bounded_rows "$task_table")"
  while read -r _task_index task_local _task_remote task_state _task_rest; do
    [[ "$task_state" == 0A ]] || continue
    case "${task_local##*:}" in
      1F90) task_management=$((task_management + 1)) ;;
      46A0) task_recovery=$((task_recovery + 1)) ;;
    esac
  done <<<"$task_tcp_rows"
done
task_recovery_mode=false
[[ "${STALWART_RECOVERY_MODE+x}" != x ]] || task_recovery_mode=true
task_http="$(curl --disable --noproxy '*' --silent --output /dev/null --write-out '%{http_code}' --connect-timeout 1 --max-time 2 http://127.0.0.1:8080/healthz/ready 2>/dev/null)" || task_http=000
[[ "$task_http" =~ ^[0-9]{3}$ ]]
task_http=$((10#$task_http))

printf '{"pid1_stalwart":%s,"uid2000":%s,"config_sha256":"%s",' "$task_pid" "$task_uid_ok" "$task_hash"
printf '"certificate_readable":%s,"key_readable":%s,"data_readable":%s,"data_writable":%s,"temporary_writable":%s,' "$task_cert" "$task_key" "$task_data_read" "$task_data_write" "$task_tmp_write"
printf '"lock_device":%s,"lock_inode":%s,"write_locks":[' "$task_device" "$task_inode"
task_separator=""
for task_identity in "${task_locks[@]}"; do
  printf '%s"%s"' "$task_separator" "$task_identity"
  task_separator=","
done
printf '],"management_listeners":%s,"recovery_listeners":%s,"recovery_mode":%s,"local_health":%s}\n' "$task_management" "$task_recovery" "$task_recovery_mode" "$task_http"
