#!/usr/bin/env bash

set -euo pipefail

usage() {
  printf 'Usage: %s sha256:<64-lowercase-hex-digits>\n' "${0##*/}" >&2
  exit 2
}

[[ "$#" -eq 1 ]] || usage
readonly digest="$1"
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || usage

readonly kubectl_bin="${KUBECTL:-kubectl}"
readonly wait_timeout="${FLUX_WAIT_TIMEOUT:-20m}"
readonly kustomization='infrastructure-controllers'
readonly revision="latest@${digest}"

kubectl_prod() {
  "${kubectl_bin}" --context admin@prod "$@"
}

kubectl_prod -n flux-system wait "kustomization/${kustomization}" \
  --for="jsonpath={.status.lastAppliedRevision}=${revision}" \
  --timeout="${wait_timeout}"
kubectl_prod -n flux-system wait "kustomization/${kustomization}" \
  --for=condition=Ready \
  --timeout="${wait_timeout}"

printf 'Flux applied the newly published %s revision and reports Ready: %s\n' \
  "${kustomization}" "${revision}"
