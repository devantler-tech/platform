#!/usr/bin/env bash
# Summarize the native Talos update plan without printing cluster details.
set -euo pipefail
umask 077

if [[ $# != 1 || "$1" != /* || ! -x "$1" ]]; then
  printf '%s\n' 'Expected the verified absolute path to the Talos lifecycle KSail executable.' >&2
  exit 1
fi

kubeconfig="${KUBECONFIG:-}"
if [[ "$kubeconfig" != /* || ! -f "$kubeconfig" || ! -s "$kubeconfig" ]]; then
  printf '%s\n' 'Expected one scoped kubeconfig file for the Talos lifecycle preview.' >&2
  exit 1
fi

scratch="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/talos-pool-layout.XXXXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT
bash scripts/render-talos-storage-pool-layout.sh >"$scratch/ksail.production.yaml"
status=0
"$1" --config "$scratch/ksail.production.yaml" cluster update --kubeconfig "$kubeconfig" --dry-run --output json \
  >"$scratch/plan.json" 2>"$scratch/diagnostics.log" || status=$?
if ((status != 0)); then
  # The native client may report credentials or endpoints alongside an error.
  # Send fixed classifications to the workflow log and discard the source text.
  case "$(<"$scratch/diagnostics.log")" in
    *"is forbidden: User "* | *"secrets is forbidden"* | *"the server has asked for the client to provide credentials"*)
      reason='Kubernetes authorization'
      ;;
    *"failed to create Talos provisioner: create Hetzner provider"* | *"hetzner API token not set"*)
      reason='Hetzner provider initialization'
      ;;
    *"connection refused"* | *"connection reset"* | *"context deadline exceeded"* | *"i/o timeout"* | *"certificate signed by unknown authority"*)
      reason='endpoint connection'
      ;;
    *"detect components"* | *"failed to check release history"*)
      reason='Kubernetes component discovery'
      ;;
    *"upgrade path"* | *"version reconciliation"* | *"detect current Talos version"*)
      reason='cluster version discovery'
      ;;
    *"failed to retrieve machine config"* | *"failed to get machine config"* | *"autoscaler pool patch"*)
      reason='Talos machine configuration'
      ;;
    *"could not retrieve current cluster configuration"*)
      reason='current cluster configuration'
      ;;
    *"invalid autoscaler configuration"* | *"failed to load configuration"* | *"invalid configuration"*)
      reason='desired cluster configuration'
      ;;
    *) reason='unclassified error' ;;
  esac
  printf 'Talos lifecycle preview failed (%s, exit %d).\n' "$reason" "$status" >&2
  exit "$status"
fi

jq -es '
  def change_list: type == "array" and all(.[]; type == "object" and
    (.field | type == "string" and length > 0) and
    (.category | type == "string" and length > 0));
  if length != 1 then error("expected one KSail update plan") else .[0] end |
  if (.totalChanges | type == "number") and (.requiresConfirmation | type == "boolean") and
      all([.inPlaceChanges,.rebootRequired,.recreateRequired,.rollingRecreate,.wipeRequired,.unknownBaseline][]; change_list) and
      .totalChanges == ([.inPlaceChanges,.rebootRequired,.recreateRequired,.rollingRecreate,.wipeRequired] | map(length) | add)
    then {
      inPlace:(.inPlaceChanges|length),
      reboot:(.rebootRequired|length),
      recreate:(.recreateRequired|length),
      rollingRecreate:(.rollingRecreate|length),
      wipe:(.wipeRequired|length),
      unknown:(.unknownBaseline|length)
    }
    else error("incomplete KSail update plan") end
' "$scratch/plan.json"
