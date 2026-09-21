#!/usr/bin/env bash
# Decide whether a controlled control-plane recreate drill may start right now.
#
# Why this exists
# ---------------
# The drill (#2764) deletes and recreates one production control-plane node to
# prove that the stable API endpoint (#2120) keeps Kubernetes reachable without
# a credential refresh. Deleting a control-plane node is the most destructive
# routine operation the platform has, so "the cluster looks healthy" must not be
# the go signal. This script turns the go signal into a fixed, tested decision.
#
# It is read-only and takes all state as input, so every branch is testable with
# fixtures and the same decision runs in CI and by hand. It reports every failing
# guard, not only the first, so one run tells the operator everything to fix.
#
# Guards
# ------
#   target  exactly one named node, which exists, carries the control-plane role
#           and is Ready.
#   quorum  exactly three control-plane nodes, all Ready, and an etcd membership
#           of three voting members with no learners. Losing one of three keeps
#           quorum; starting from anything less does not.
#   backup  the newest backup from the daily Velero schedule is Completed and
#           finished within the age limit. PartiallyFailed is not Completed: it
#           means some volumes are missing from the backup the drill would fall
#           back on.
#
# Recovery is deliberately not checked here. A caller-asserted "recovery is
# ready" flag would prove nothing; recovery belongs to the drill itself.
#
# The node holding the floating IP is reported but never gates the decision: the
# drill may target it on purpose, because that is the failover case.
#
# Inputs
# ------
#   --target NODE               node the drill would recreate (required)
#   --nodes FILE                `kubectl get nodes -o json` (required)
#   --backups FILE              `kubectl get backups.velero.io -n velero -o json` (required)
#   --etcd-members N            voting etcd members, e.g. counted from
#                               `talosctl etcd members` (required)
#   --etcd-learners N           etcd learner members (required)
#   --vip-holder NODE           node the floating IP is assigned to (optional)
#   --schedule NAME             Velero schedule to read (default velero-daily-full)
#   --max-backup-age-hours N    newest backup age limit (default 26)
#   --now EPOCH_SECONDS         evaluation time (default: current time)
#
# Output (stdout, key=value) and exit status
# ------------------------------------------
#   ready=true|false, target=..., target_is_vip_holder=true|false|unknown,
#   and one `reason=<guard>: <detail>` line per failing guard.
#   0  ready: every guard passed
#   1  not ready: at least one guard failed
#   2  input error: missing, unreadable or malformed input. Never `ready`.

set -euo pipefail

readonly expected_control_planes=3

die_input() {
  printf 'cp-recreate-drill-preflight: %s\n' "$1" >&2
  printf 'ready=false\n'
  exit 2
}

is_count() {
  [[ "$1" =~ ^[0-9]+$ ]]
}

target=""
nodes_file=""
backups_file=""
etcd_members=""
etcd_learners=""
vip_holder=""
schedule="velero-daily-full"
max_age_hours="26"
now=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --target | --nodes | --backups | --etcd-members | --etcd-learners | --vip-holder | --schedule | --max-backup-age-hours | --now)
      [ "$#" -ge 2 ] || die_input "$1 needs a value"
      case "$1" in
        --target) target="$2" ;;
        --nodes) nodes_file="$2" ;;
        --backups) backups_file="$2" ;;
        --etcd-members) etcd_members="$2" ;;
        --etcd-learners) etcd_learners="$2" ;;
        --vip-holder) vip_holder="$2" ;;
        --schedule) schedule="$2" ;;
        --max-backup-age-hours) max_age_hours="$2" ;;
        --now) now="$2" ;;
      esac
      shift 2
      ;;
    *) die_input "unknown argument: $1" ;;
  esac
done

[ -n "${target}" ] || die_input "--target is required"
[ -n "${schedule}" ] || die_input "--schedule must not be empty"
[ -r "${nodes_file}" ] || die_input "--nodes file is missing or unreadable"
[ -r "${backups_file}" ] || die_input "--backups file is missing or unreadable"
is_count "${etcd_members}" || die_input "--etcd-members must be a non-negative integer"
is_count "${etcd_learners}" || die_input "--etcd-learners must be a non-negative integer"
is_count "${max_age_hours}" || die_input "--max-backup-age-hours must be a non-negative integer"
if [ -z "${now}" ]; then
  now="$(date -u +%s)"
fi
is_count "${now}" || die_input "--now must be epoch seconds"
# Read every count as base 10: shell arithmetic takes a leading zero as octal,
# so "08" would abort the script and "010" would silently mean eight.
etcd_members=$((10#${etcd_members}))
etcd_learners=$((10#${etcd_learners}))
max_age_hours=$((10#${max_age_hours}))
now=$((10#${now}))

jq -e '.kind == "List" or .kind == "NodeList"' "${nodes_file}" >/dev/null 2>&1 ||
  die_input "--nodes is not a node list"
jq -e '(.items | type) == "array"' "${nodes_file}" >/dev/null 2>&1 ||
  die_input "--nodes has no items array"
jq -e '(.items | type) == "array"' "${backups_file}" >/dev/null 2>&1 ||
  die_input "--backups has no items array"
# Every Node carries a name. An item without one would silently drop out of both
# the control-plane count and the target lookup, reading as a smaller cluster
# rather than as the malformed input it is.
jq -e '.items | all(type == "object" and (.metadata | type) == "object" and (.metadata.name | type) == "string")' \
  "${nodes_file}" >/dev/null 2>&1 || die_input "--nodes has an item without metadata.name"
# Every Backup carries a name and a creation time, which is what orders them.
# Phase, completion time and schedule label are legitimately absent on an
# unfinished or unscheduled backup, so they are not required here.
jq -e '.items | all(type == "object" and (.metadata | type) == "object"
  and (.metadata.name | type) == "string" and (.metadata.creationTimestamp | type) == "string"
  and (.status == null or (.status | type) == "object"))' \
  "${backups_file}" >/dev/null 2>&1 || die_input "--backups has an item without metadata.name or metadata.creationTimestamp"

# One row per control-plane node: "<name> <Ready status>". A node without a
# Ready condition reports "Unknown", which is not Ready.
control_planes="$(jq -r '
  .items[]
  | select(.metadata.labels | has("node-role.kubernetes.io/control-plane"))
  | "\(.metadata.name) \((.status.conditions // []) | map(select(.type == "Ready")) | (.[0].status // "Unknown"))"
' "${nodes_file}")" || die_input "--nodes could not be read"

reasons=()

# target
target_row="$(jq -r --arg t "${target}" '
  [.items[] | select(.metadata.name == $t)] | if length == 0 then "absent"
  else .[0] | "\(.metadata.labels | has("node-role.kubernetes.io/control-plane")) \((.status.conditions // []) | map(select(.type == "Ready")) | (.[0].status // "Unknown"))"
  end
' "${nodes_file}")" || die_input "--nodes could not be read"
case "${target_row}" in
  absent) reasons+=("target: ${target} is not a node in the cluster") ;;
  "false "*) reasons+=("target: ${target} is not a control-plane node") ;;
  "true True") ;;
  *) reasons+=("target: ${target} is not Ready (${target_row#* })") ;;
esac

# quorum
control_plane_count=0
not_ready=()
while IFS=' ' read -r name ready; do
  [ -n "${name}" ] || continue
  control_plane_count=$((control_plane_count + 1))
  [ "${ready}" = "True" ] || not_ready+=("${name}")
done <<<"${control_planes}"
if [ "${control_plane_count}" -ne "${expected_control_planes}" ]; then
  reasons+=("quorum: ${control_plane_count} control-plane nodes, expected ${expected_control_planes}")
fi
if [ "${#not_ready[@]}" -gt 0 ]; then
  reasons+=("quorum: control-plane nodes not Ready: ${not_ready[*]}")
fi
if [ "${etcd_members}" -ne "${expected_control_planes}" ]; then
  reasons+=("quorum: ${etcd_members} voting etcd members, expected ${expected_control_planes}")
fi
if [ "${etcd_learners}" -ne 0 ]; then
  reasons+=("quorum: ${etcd_learners} etcd learners present; a membership change is still in progress")
fi

# backup
# The newest backup is chosen by creation time, which every backup has from the
# moment it starts. Choosing among finished backups only would pass over a newer
# backup still in progress and approve the drill on an older one.
newest="$(jq -r --arg s "${schedule}" '
  [.items[]
   | select((.metadata.labels // {})["velero.io/schedule-name"] == $s)
   | {name: .metadata.name, created: (.metadata.creationTimestamp | fromdateiso8601),
      phase: (.status.phase // "Unknown"),
      completed: (.status.completionTimestamp // null | if . == null then "none" else fromdateiso8601 end)}]
  | sort_by(.created)
  | if length == 0 then "none"
    else .[-1] | "\(.name) \(.phase) \(.completed)"
    end
' "${backups_file}")" || die_input "--backups could not be read (malformed creationTimestamp or completionTimestamp?)"
if [ "${newest}" = "none" ]; then
  reasons+=("backup: no backup from schedule ${schedule}")
else
  read -r backup_name backup_phase backup_epoch <<<"${newest}"
  if [ "${backup_phase}" != "Completed" ]; then
    reasons+=("backup: newest ${schedule} backup ${backup_name} is ${backup_phase}, not Completed")
  fi
  if [ "${backup_epoch}" = "none" ]; then
    reasons+=("backup: newest ${schedule} backup ${backup_name} has not finished")
  else
    age_seconds=$((now - backup_epoch))
    # The limit is exclusive: a backup exactly max_age_hours old is too old.
    if [ "${age_seconds}" -ge $((max_age_hours * 3600)) ]; then
      reasons+=("backup: newest ${schedule} backup ${backup_name} finished $((age_seconds / 3600))h ago, limit ${max_age_hours}h")
    fi
  fi
fi

if [ -z "${vip_holder}" ]; then
  vip_state="unknown"
elif [ "${vip_holder}" = "${target}" ]; then
  vip_state="true"
else
  vip_state="false"
fi

printf 'target=%s\n' "${target}"
printf 'target_is_vip_holder=%s\n' "${vip_state}"
if [ "${#reasons[@]}" -eq 0 ]; then
  printf 'ready=true\n'
  exit 0
fi
printf 'ready=false\n'
printf 'reason=%s\n' "${reasons[@]}"
exit 1
