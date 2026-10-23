#!/bin/sh
# Coroot scopes queries against a shared Prometheus by its database project ID.
# Discover that ID on every snapshot so restores and project recreation recover.
set -eu
base=${COROOT_URL:-http://coroot-coroot.observability.svc.cluster.local.:8080}
project=${COROOT_PROJECT:-platform}
project_response=$(curl --silent --show-error --fail --connect-timeout 5 --max-time 15 \
  "${base%/}/api/user") || {
  echo 'Coroot project binding is unknown' >&2
  exit 1
}
project_id=$(printf '%s' "${project_response}" |
  jq -er --arg name "${project}" '
    .projects | select(type == "array")
    | [.[] | select(.name == $name) | .id]
    | select(length == 1) | .[0]
    | select(type == "string" and test("^[A-Za-z0-9_-]{1,128}$"))
  ' 2>/dev/null) || {
  echo 'Coroot project binding is unknown' >&2
  exit 1
}
# ID validation above makes this an inert label value. Never copy credentials,
# API response bodies or project names into the metrics stream.
awk -v project="${project_id}" '
  /^[[:space:]]*#/ || /^[[:space:]]*$/ { print; next }
  /^[A-Za-z_:][A-Za-z0-9_:]*\{/ {
    if ($0 ~ /[{,]coroot_project_id=/) exit 1
    sub(/\{/, "{coroot_project_id=\"" project "\",")
    print; next
  }
  /^[A-Za-z_:][A-Za-z0-9_:]*[[:space:]]/ {
    sub(/[[:space:]]/, "{coroot_project_id=\"" project "\"} ")
    print; next
  }
  { exit 1 }
'
