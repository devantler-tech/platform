#!/usr/bin/env bash
# Probe publication health, independently of the security verdict it reports.
set -euo pipefail

require_project=false
case "${1:-}" in
"") [[ $# == 0 ]] || exit 2 ;;
--require-project)
  [[ $# == 1 ]] || exit 2
  require_project=true
  ;;
*) exit 2 ;;
esac

metrics_file=${METRICS_FILE:-/metrics/metrics}
[[ -s "$metrics_file" ]] || exit 1
modified=$(stat -c %Y "$metrics_file" 2>/dev/null || stat -f %m "$metrics_file")
[[ "$modified" =~ ^[0-9]+$ ]] || exit 1
age=$(($(date +%s) - modified))
((age >= 0 && age <= 900)) || exit 1
"$require_project" || exit 0

# Only these two scalar series establish a complete publication. Accept either
# label order, but reject duplicates, malformed IDs and different projects.
project_pattern='[A-Za-z0-9_-]{1,128}'
project_first="^platform_security_collection_success\\{coroot_project_id=\"($project_pattern)\",surface=\"(policy|kubescape)\"\\} [01]$"
surface_first="^platform_security_collection_success\\{surface=\"(policy|kubescape)\",coroot_project_id=\"($project_pattern)\"\\} [01]$"
project_id='' policy_seen=false kubescape_seen=false
while IFS= read -r line || [[ -n "$line" ]]; do
  [[ "$line" == platform_security_collection_success* ]] || continue
  if [[ "$line" =~ $project_first ]]; then
    current_project=${BASH_REMATCH[1]}
    surface=${BASH_REMATCH[2]}
  elif [[ "$line" =~ $surface_first ]]; then
    surface=${BASH_REMATCH[1]}
    current_project=${BASH_REMATCH[2]}
  else
    exit 1
  fi
  [[ -z "$project_id" || "$project_id" == "$current_project" ]] || exit 1
  project_id=$current_project
  case "$surface" in
  policy)
    "$policy_seen" && exit 1
    policy_seen=true
    ;;
  kubescape)
    "$kubescape_seen" && exit 1
    kubescape_seen=true
    ;;
  esac
done <"$metrics_file"
"$policy_seen" && "$kubescape_seen"
