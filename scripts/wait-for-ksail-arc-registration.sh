#!/usr/bin/env bash
# Read-only convergence polling; the caller also imposes a 660-second hard deadline.
set -euo pipefail
[[ "$#" -eq 1 && "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || exit 2
[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform ]] || exit 1
readonly revision="latest@$1"
readonly deadline=$((SECONDS + 600))
kc() { timeout 35s kubectl --context admin@prod --request-timeout=30s "$@" 2>/dev/null; }

while ((SECONDS < deadline)); do
  ready=true
  for layer in infrastructure apps; do
    if ! state=$(kc -n flux-system get kustomization "$layer" -o json) ||
      ! jq -e --arg revision "$revision" '
        .status.lastAppliedRevision == $revision and
        .status.observedGeneration == .metadata.generation and
        any(.status.conditions[]; .type == "Ready" and .status == "True")
      ' <<<"$state" >/dev/null 2>&1; then
      ready=false
    fi
  done
  if ! state=$(kc -n arc-ksail-analysis get autoscalingrunnerset.actions.github.com ksail-code-quality -o json) ||
    ! jq -e '
      .status.phase == "Running" and .status.observedGeneration == .metadata.generation and
      ((.metadata.annotations["runner-scale-set-id"] | tonumber) > 0)
    ' <<<"$state" >/dev/null 2>&1; then
    ready=false
  fi
  if "$ready"; then
    printf 'ARC layers and registration converged at the deployed revision\n'
    exit 0
  fi
  sleep 10
done
printf 'ARC layers or registration did not converge before the deadline\n' >&2
exit 1
