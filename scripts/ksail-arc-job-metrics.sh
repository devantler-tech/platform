#!/usr/bin/env bash
set -euo pipefail

arc_metrics_fail() {
  printf '::error::KSail ARC whole-job cgroup measurement failed: %s\n' "$1" >&2
  return 1
}

arc_metrics_read() {
  local root=$1 name=$2
  if [[ "$root" == /sys/fs/cgroup ]]; then
    timeout 5s cat "$root/$name" 2>/dev/null
  else
    # Only regular test-owned fixture files use the portable read path.
    [[ -f "$root/$name" && ! -L "$root/$name" ]] || return 1
    cat "$root/$name" 2>/dev/null
  fi
}

# The production entrypoint uses only the container's kernel cgroup. A supplied
# fixture directory is used when this function is sourced by offline tests.
arc_job_metrics() {
  local root=$1 limit peak events key value extra
  local low='' high='' max='' oom='' oom_kill='' oom_group_kill='' sock_throttled=''
  limit=$(arc_metrics_read "$root" memory.max) || { arc_metrics_fail read-limit; return 1; }
  peak=$(arc_metrics_read "$root" memory.peak) || { arc_metrics_fail read-peak; return 1; }
  events=$(arc_metrics_read "$root" memory.events) || { arc_metrics_fail read-events; return 1; }
  [[ "$limit" == 15032385536 ]] || { arc_metrics_fail limit; return 1; }
  [[ "$peak" =~ ^(0|[1-9][0-9]{0,17})$ && "$peak" -gt 0 ]] || {
    arc_metrics_fail peak; return 1;
  }
  while read -r key value extra; do
    [[ -z "$extra" && "$value" =~ ^(0|[1-9][0-9]{0,17})$ ]] || { arc_metrics_fail event-format; return 1; }
    case "$key" in
      low|high|max|oom|oom_kill|oom_group_kill)
        [[ -z "${!key}" ]] || { arc_metrics_fail duplicate-event; return 1; }
        printf -v "$key" '%s' "$value" ;;
      sock_throttled)
        [[ -z "$sock_throttled" ]] || { arc_metrics_fail duplicate-event; return 1; }
        sock_throttled=$value ;;
      *) arc_metrics_fail unknown-event; return 1 ;;
    esac
  done <<<"$events"
  [[ -n "$low" && -n "$high" && -n "$max" && -n "$oom" && -n "$oom_kill" && -n "$oom_group_kill" ]] || {
    arc_metrics_fail missing-event; return 1;
  }
  [[ "$oom" == 0 && "$oom_kill" == 0 && "$oom_group_kill" == 0 ]] || { arc_metrics_fail oom; return 1; }
  printf 'KSail ARC whole-job cgroup: {"schemaVersion":1,"memoryMaxBytes":%s,"memoryPeakBytes":%s,"limitEvents":%s,"oomEvents":%s,"oomKills":%s,"oomGroupKills":%s}\n' \
    "$limit" "$peak" "$max" "$oom" "$oom_kill" "$oom_group_kill"
  # The kernel may temporarily exceed memory.max. Retain that valid measurement,
  # while preserving the existing runtime acceptance ceiling as a separate refusal.
  [[ "$peak" -le "$limit" ]] || { arc_metrics_fail budget; return 1; }
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  [[ $# == 0 ]] || { arc_metrics_fail arguments; exit 1; }
  arc_job_metrics /sys/fs/cgroup
fi
