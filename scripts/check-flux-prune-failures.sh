#!/usr/bin/env bash
# Fail a deploy whose Flux layers could not delete an object they tried to
# prune at the revision this deploy published (#4734).
#
# Flux reports a refused delete as a `PruneFailed` event and still goes on to
# `ReconciliationSucceeded`, so the reconcile step and the exact-revision wait
# both pass while the removed object keeps running, unmanaged. This reads the
# Kustomization events in flux-system, keeps only those whose
# `kustomize.toolkit.fluxcd.io/revision` annotation names this deploy's
# revision, and fails when any of them is a `PruneFailed`.
#
# Selecting by revision rather than by time keeps an older deploy's failure out
# of this one, and a quiet cluster does not matter: the platform layers record
# events only when they apply a new revision, and every deploy publishes one.
# That is also how the check proves it can see events at all: if no event from
# a platform layer carries this revision, the read is treated as a failure,
# never as a clean prune.
#
# Only the object identity is printed, never the rest of Flux's message.

set -euo pipefail

usage() {
  printf 'Usage: %s sha256:<64-lowercase-hex-digits>\n' "${0##*/}" >&2
  exit 2
}

[[ "$#" -eq 1 ]] || usage
readonly digest="$1"
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || usage

readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly revision="latest@${digest}"
# The root sync and the four layers it composes (k8s/clusters/base).
readonly layers_json='["flux-system","bootstrap","infrastructure-controllers","infrastructure","apps"]'

if ! events="$("${kubectl_bin}" --context admin@prod -n flux-system get events \
  --field-selector involvedObject.kind=Kustomization -o json)"; then
  printf '::error::Could not read the Flux Kustomization events, so a failed prune cannot be ruled out.\n' >&2
  exit 1
fi

# One TSV row per event at this revision: layer, reason, then one identity per
# message line that starts `Kind/namespace/name` or `Kind/name` (or `-` when no
# line names an object).
if ! rows="$(jq -r --arg revision "${revision}" --argjson layers "${layers_json}" '
  [.items[]?
    | select(.involvedObject.name as $name | $layers | index($name))
    | select((.metadata.annotations // {})["kustomize.toolkit.fluxcd.io/revision"] == $revision)]
  | .[]
  | [.involvedObject.name, (.reason // ""),
     ([(.message // "") | split("\n")[]
       | capture("^(?<id>[A-Z][A-Za-z0-9]*(/[a-z0-9][a-z0-9.-]*){1,2})(\\s|$)")?
       | .id] | if length == 0 then "-" else join(",") end)]
  | @tsv' <<<"${events}")"; then
  printf '::error::The Flux Kustomization events could not be parsed, so a failed prune cannot be ruled out.\n' >&2
  exit 1
fi

if [[ -z "${rows}" ]]; then
  printf '::error::No Flux layer recorded an event for %s, so this check cannot see whether a prune failed.\n' \
    "${revision}" >&2
  exit 1
fi

seen=0
failed=0
while IFS=$'\t' read -r layer reason objects; do
  seen=$((seen + 1))
  [[ "${reason}" == 'PruneFailed' ]] || continue
  failed=$((failed + 1))
  if [[ "${objects}" == '-' ]]; then
    printf '::error::Flux layer %s could not delete an object it was pruning, and its event does not name the object.\n' \
      "${layer}" >&2
  else
    printf '::error::Flux layer %s could not delete %s, which is no longer in the repository but still runs in the cluster.\n' \
      "${layer}" "${objects//,/, }" >&2
  fi
done <<<"${rows}"

if [[ "${failed}" -gt 0 ]]; then
  printf 'Flux reported %d failed prune(s) for %s.\n' "${failed}" "${revision}" >&2
  exit 1
fi

printf 'No Flux layer reported a failed prune for %s (%d event(s) checked).\n' "${revision}" "${seen}"
