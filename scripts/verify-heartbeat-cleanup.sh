#!/usr/bin/env bash
# Read only fixed key-presence predicates. Never decode, print or contact a URL.
set +x
set -euo pipefail
umask 077

unverified() { printf '%s\n' 'HEARTBEAT_CLEANUP=UNVERIFIED'; exit 2; }
if [[ ${GITHUB_EVENT_NAME:-} != workflow_dispatch || ${GITHUB_REF:-} != refs/heads/main || ${HEARTBEAT_CLEANUP_CONFIRM:-} != verify-heartbeat-cleanup ]]; then
  unverified
fi
for tool in kubectl jq mktemp rm; do
  command -v "$tool" >/dev/null 2>&1 || unverified
done
private_dir=$(mktemp -d) || unverified
trap 'rm -rf "$private_dir"' EXIT

if ! kubectl --context admin@prod --request-timeout=20s -n flux-system get secret variables-cluster -o json > "$private_dir/original.json" 2>/dev/null; then
  unverified
fi
if ! jq -e '
  .apiVersion == "v1" and .kind == "Secret"
  and .metadata.name == "variables-cluster" and .metadata.namespace == "flux-system"
  and (.data|type) == "object"
  and (.data|has("alertmanager_heartbeat_url")|not)
  and ((.stringData // {})|has("alertmanager_heartbeat_url")|not)
' "$private_dir/original.json" >/dev/null 2>&1; then
  unverified
fi
if ! kubectl --context admin@prod --request-timeout=20s -n flux-system get secret variables-heartbeat -o json > "$private_dir/replacement.json" 2>/dev/null; then
  unverified
fi
if ! jq -e '
  .apiVersion == "v1" and .kind == "Secret"
  and .metadata.name == "variables-heartbeat" and .metadata.namespace == "flux-system"
  and (.data|type) == "object"
  and (.data|keys) == ["alertmanager_heartbeat_url"]
  and (.data.alertmanager_heartbeat_url|type == "string" and length > 0)
  and ((.stringData // {})|length) == 0
' "$private_dir/replacement.json" >/dev/null 2>&1; then
  unverified
fi
printf '%s\n' 'HEARTBEAT_CLEANUP=VERIFIED'
