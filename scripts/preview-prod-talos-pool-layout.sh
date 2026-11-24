#!/usr/bin/env bash
# Summarize the native Talos update plan without printing cluster details.
set -euo pipefail
umask 077

if [[ $# != 1 || "$1" != /* || ! -x "$1" ]]; then
  printf '%s\n' 'Expected the verified absolute path to the Talos lifecycle KSail executable.' >&2
  exit 1
fi

scratch="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/talos-pool-layout.XXXXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT
bash scripts/render-talos-storage-pool-layout.sh >"$scratch/ksail.production.yaml"
if ! "$1" --config "$scratch/ksail.production.yaml" cluster update --dry-run --output json \
  >"$scratch/plan.json" 2>"$scratch/diagnostics.log"; then
  printf '%s\n' 'The Talos lifecycle preview did not complete.' >&2
  exit 1
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
