#!/usr/bin/env bash
# Behaviour tests for scripts/cp-recreate-drill-preflight.sh.
#
# Every case starts from one healthy baseline and breaks exactly one guard, so a
# refusal can only come from the guard under test. The baseline itself must be
# `ready`: without that control, a script that always refuses would pass every
# refusal case.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly preflight="${root_dir}/scripts/cp-recreate-drill-preflight.sh"

# 2026-09-21T12:00:00Z
readonly now=1789992000

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

ok() {
  pass_count=$((pass_count + 1))
  printf 'ok — %s\n' "$1"
}

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

# node <name> <control-plane: yes|no> <Ready status|none>
node() {
  local labels='{}' conditions='[]'
  [ "$2" = "yes" ] && labels='{"node-role.kubernetes.io/control-plane": ""}'
  [ "$3" != "none" ] && conditions="[{\"type\": \"Ready\", \"status\": \"$3\"}]"
  printf '{"metadata": {"name": "%s", "labels": %s}, "status": {"conditions": %s}}' "$1" "${labels}" "${conditions}"
}

# backup <name> <phase> <completionTimestamp|null> [schedule] [creationTimestamp]
# A backup is created before it completes; by default it was created at its
# completion time (or at 03:00Z on the evaluation day when it has not finished).
backup() {
  local completion=null created="${5:-${3}}"
  [ "$3" != "null" ] && completion="\"$3\""
  [ "${created}" = "null" ] && created="2026-09-21T03:00:00Z"
  printf '{"metadata": {"name": "%s", "creationTimestamp": "%s", "labels": {"velero.io/schedule-name": "%s"}}, "status": {"phase": "%s", "completionTimestamp": %s}}' \
    "$1" "${created}" "${4-velero-daily-full}" "$2" "${completion}"
}

list() {
  local IFS=,
  printf '{"kind": "List", "items": [%s]}' "$*"
}

healthy_nodes="$(list \
  "$(node prod-control-plane-2 yes True)" \
  "$(node prod-control-plane-4 yes True)" \
  "$(node prod-control-plane-5 yes True)" \
  "$(node prod-worker-1 no True)")"
healthy_backups="$(list \
  "$(backup daily-old PartiallyFailed 2026-09-19T02:40:00Z)" \
  "$(backup daily-new Completed 2026-09-21T02:40:00Z)")"

# run <nodes-json> <backups-json> [extra args...] — prints stdout, returns status.
run() {
  printf '%s' "$1" >"${work_dir}/nodes.json"
  printf '%s' "$2" >"${work_dir}/backups.json"
  shift 2
  local status=0
  "${preflight}" --nodes "${work_dir}/nodes.json" --backups "${work_dir}/backups.json" \
    --now "${now}" "$@" >"${work_dir}/out" 2>"${work_dir}/err" || status=$?
  return "${status}"
}

defaults=(--target prod-control-plane-4 --etcd-members 3 --etcd-learners 0)

# expect_refusal <description> <reason substring> <nodes> <backups> [args...]
expect_refusal() {
  local description="$1" reason="$2" status=0
  shift 2
  run "$@" || status=$?
  [ "${status}" -eq 1 ] || fail "${description}: expected exit 1, got ${status}"
  grep -qx 'ready=false' "${work_dir}/out" || fail "${description}: ready=false missing"
  grep -qF "reason=${reason}" "${work_dir}/out" ||
    fail "${description}: reason '${reason}' missing from: $(cat "${work_dir}/out")"
  ok "${description}"
}

# expect_input_error <description> <nodes> <backups> [args...]
expect_input_error() {
  local description="$1" status=0
  shift
  run "$@" || status=$?
  [ "${status}" -eq 2 ] || fail "${description}: expected exit 2, got ${status}"
  if grep -qx 'ready=true' "${work_dir}/out"; then
    fail "${description}: input error reported ready=true"
  fi
  ok "${description}"
}

# Control: the healthy baseline is ready.
status=0
run "${healthy_nodes}" "${healthy_backups}" "${defaults[@]}" || status=$?
[ "${status}" -eq 0 ] || fail "healthy baseline: expected exit 0, got ${status}: $(cat "${work_dir}/out" "${work_dir}/err")"
grep -qx 'ready=true' "${work_dir}/out" || fail "healthy baseline: ready=true missing"
grep -q '^reason=' "${work_dir}/out" && fail "healthy baseline: printed a reason"
grep -qx 'target_is_vip_holder=unknown' "${work_dir}/out" || fail "healthy baseline: VIP holder should be unknown"
ok "healthy baseline is ready"

# The VIP holder is reported, never a gate.
run "${healthy_nodes}" "${healthy_backups}" "${defaults[@]}" --vip-holder prod-control-plane-4 ||
  fail "VIP holder as target must still be ready"
grep -qx 'target_is_vip_holder=true' "${work_dir}/out" || fail "VIP holder not reported"
run "${healthy_nodes}" "${healthy_backups}" "${defaults[@]}" --vip-holder prod-control-plane-2 ||
  fail "VIP elsewhere must still be ready"
grep -qx 'target_is_vip_holder=false' "${work_dir}/out" || fail "VIP elsewhere not reported"
ok "VIP holder is reported and does not gate"

# target
expect_refusal "absent target" "target: prod-control-plane-9 is not a node" \
  "${healthy_nodes}" "${healthy_backups}" --target prod-control-plane-9 --etcd-members 3 --etcd-learners 0
expect_refusal "worker as target" "target: prod-worker-1 is not a control-plane node" \
  "${healthy_nodes}" "${healthy_backups}" --target prod-worker-1 --etcd-members 3 --etcd-learners 0

not_ready_target="$(list \
  "$(node prod-control-plane-2 yes True)" \
  "$(node prod-control-plane-4 yes False)" \
  "$(node prod-control-plane-5 yes True)")"
expect_refusal "target not Ready" "target: prod-control-plane-4 is not Ready (False)" \
  "${not_ready_target}" "${healthy_backups}" "${defaults[@]}"

# quorum
other_not_ready="$(list \
  "$(node prod-control-plane-2 yes True)" \
  "$(node prod-control-plane-4 yes True)" \
  "$(node prod-control-plane-5 yes none)")"
expect_refusal "another control plane without a Ready condition" \
  "quorum: control-plane nodes not Ready: prod-control-plane-5" \
  "${other_not_ready}" "${healthy_backups}" "${defaults[@]}"

two_control_planes="$(list \
  "$(node prod-control-plane-2 yes True)" \
  "$(node prod-control-plane-4 yes True)")"
expect_refusal "only two control planes" "quorum: 2 control-plane nodes, expected 3" \
  "${two_control_planes}" "${healthy_backups}" "${defaults[@]}"

expect_refusal "etcd membership short" "quorum: 2 voting etcd members, expected 3" \
  "${healthy_nodes}" "${healthy_backups}" --target prod-control-plane-4 --etcd-members 2 --etcd-learners 0
four_control_planes="$(list \
  "$(node prod-control-plane-2 yes True)" \
  "$(node prod-control-plane-4 yes True)" \
  "$(node prod-control-plane-5 yes True)" \
  "$(node prod-control-plane-6 yes True)")"
expect_refusal "four control planes" "quorum: 4 control-plane nodes, expected 3" \
  "${four_control_planes}" "${healthy_backups}" "${defaults[@]}"
expect_refusal "etcd membership over" "quorum: 4 voting etcd members, expected 3" \
  "${healthy_nodes}" "${healthy_backups}" --target prod-control-plane-4 --etcd-members 4 --etcd-learners 0
expect_refusal "etcd learner present" "quorum: 1 etcd learners present" \
  "${healthy_nodes}" "${healthy_backups}" --target prod-control-plane-4 --etcd-members 3 --etcd-learners 1

# backup
partially_failed="$(list \
  "$(backup daily-old Completed 2026-09-20T02:40:00Z)" \
  "$(backup daily-new PartiallyFailed 2026-09-21T02:40:00Z)")"
expect_refusal "newest backup PartiallyFailed" \
  "backup: newest velero-daily-full backup daily-new is PartiallyFailed, not Completed" \
  "${healthy_nodes}" "${partially_failed}" "${defaults[@]}"

stale="$(list "$(backup daily-stale Completed 2026-09-20T02:40:00Z)")"
expect_refusal "newest backup too old" "backup: newest velero-daily-full backup daily-stale finished 33h ago, limit 26h" \
  "${healthy_nodes}" "${stale}" "${defaults[@]}"

in_progress_only="$(list "$(backup daily-running InProgress null)")"
expect_refusal "only backup still running" "backup: newest velero-daily-full backup daily-running has not finished" \
  "${healthy_nodes}" "${in_progress_only}" "${defaults[@]}"

expect_refusal "no backup from the schedule" "backup: no backup from schedule velero-daily-full" \
  "${healthy_nodes}" "$(list "$(backup manual Completed 2026-09-21T11:00:00Z adhoc)")" "${defaults[@]}"

# A newer backup still running is the newest backup, even though an older one
# finished: the drill must not start on the older one while the newer is unproven.
newer_unfinished="$(list \
  "$(backup daily-new Completed 2026-09-21T02:40:00Z velero-daily-full 2026-09-21T02:17:00Z)" \
  "$(backup daily-newer InProgress null velero-daily-full 2026-09-21T11:00:00Z)")"
expect_refusal "newer backup still in progress" \
  "backup: newest velero-daily-full backup daily-newer is InProgress, not Completed" \
  "${healthy_nodes}" "${newer_unfinished}" "${defaults[@]}"

# A valid backup may lack phase and completion while it starts: not ready, not malformed.
incomplete_valid='{"kind": "List", "items": [{"metadata": {"name": "daily-starting", "creationTimestamp": "2026-09-21T11:30:00Z", "labels": {"velero.io/schedule-name": "velero-daily-full"}}, "status": {}}]}'
expect_refusal "incomplete but valid backup" "backup: newest velero-daily-full backup daily-starting is Unknown, not Completed" \
  "${healthy_nodes}" "${incomplete_valid}" "${defaults[@]}"

other_schedule="$(list \
  "$(backup daily-new PartiallyFailed 2026-09-21T02:40:00Z)" \
  "$(backup manual-new Completed 2026-09-21T11:00:00Z adhoc)")"
expect_refusal "a Completed backup from another schedule does not count" \
  "backup: newest velero-daily-full backup daily-new is PartiallyFailed" \
  "${healthy_nodes}" "${other_schedule}" "${defaults[@]}"

# Every failing guard is reported in one run.
status=0
run "${two_control_planes}" "${partially_failed}" --target prod-worker-1 --etcd-members 2 --etcd-learners 0 || status=$?
[ "${status}" -eq 1 ] || fail "multiple failures: expected exit 1, got ${status}"
[ "$(grep -c '^reason=' "${work_dir}/out")" -eq 4 ] ||
  fail "multiple failures: expected 4 reasons, got: $(cat "${work_dir}/out")"
ok "every failing guard is reported in one run"

# Input errors are exit 2 and never ready.
expect_input_error "missing target" "${healthy_nodes}" "${healthy_backups}" --etcd-members 3 --etcd-learners 0
expect_input_error "non-numeric etcd count" "${healthy_nodes}" "${healthy_backups}" \
  --target prod-control-plane-4 --etcd-members three --etcd-learners 0
expect_input_error "missing etcd learners" "${healthy_nodes}" "${healthy_backups}" \
  --target prod-control-plane-4 --etcd-members 3
expect_input_error "nodes not JSON" "not json" "${healthy_backups}" "${defaults[@]}"
expect_input_error "nodes not a list" '{"kind": "Node", "items": []}' "${healthy_backups}" "${defaults[@]}"
expect_input_error "backups without items" "${healthy_nodes}" '{}' "${defaults[@]}"
expect_input_error "malformed completion timestamp" "${healthy_nodes}" \
  "$(list "$(backup daily-new Completed yesterday velero-daily-full 2026-09-21T02:17:00Z)")" "${defaults[@]}"
expect_input_error "node item without a name" \
  "$(list "$(node prod-control-plane-2 yes True)" '{}')" "${healthy_backups}" "${defaults[@]}"
expect_input_error "backup item without metadata" "${healthy_nodes}" \
  "$(list "$(backup daily-new Completed 2026-09-21T02:40:00Z)" '{}')" "${defaults[@]}"
expect_input_error "unknown argument" "${healthy_nodes}" "${healthy_backups}" "${defaults[@]}" --force

printf 'All %d cp-recreate-drill-preflight tests passed.\n' "${pass_count}"
