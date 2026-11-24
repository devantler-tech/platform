#!/usr/bin/env bash
# Keeps the platform's deploy and recovery paths off the GitHub token broker
# the platform itself hosts (devantler-tech/monorepo ADR 0020). If a workflow
# that deploys or rebuilds the platform needed a broker token, a platform
# outage would block its own repair.
#
# Fails (exit 1) when a deploy or recovery surface names the octo-sts action or
# image, or the broker's hostname. Exits 2 (UNKNOWN) when a surface is missing
# or the hostname cannot be read, so an incomplete census never passes.
#
# Usage: guard-github-broker-independence.sh [repository root]
set -euo pipefail

root="${1:-.}"
cd "${root}"
route=k8s/providers/hetzner/apps/github-broker/http-route.yaml
cluster_variables=k8s/clusters/prod/bootstrap/config-map.yaml

unknown() {
  printf 'UNKNOWN: %s\n' "$1" >&2
  exit 2
}

# The broker's hostname, as the route declares it and as production resolves it.
[[ -r "${route}" ]] || unknown "cannot read the broker route ${route}"
if ! literal_host="$(yq -e '.spec.hostnames | select(length == 1) | .[0]' "${route}" 2>/dev/null)" || [[ -z "${literal_host}" ]]; then
  unknown 'expected exactly one broker hostname'
fi
if ! domain="$(yq -e 'select(.kind == "ConfigMap" and .metadata.name == "variables-cluster") | .data.domain' "${cluster_variables}" 2>/dev/null)" || [[ -z "${domain}" ]]; then
  unknown 'cannot read the production domain'
fi
# shellcheck disable=SC2016 # the literal Flux variable, not a shell expansion
domain_variable='${domain}'
resolved_host="${literal_host//"${domain_variable}"/${domain}}"
if [[ "${resolved_host}" == "${literal_host}" || "${resolved_host}" == *'$'* ]]; then
  unknown "broker hostname ${literal_host} does not resolve through the domain variable"
fi

# The deploy and recovery surfaces. Every named file must exist, and each glob
# must match, or the census is incomplete.
surfaces=()
for path in \
  .github/workflows/cd.yaml \
  .github/workflows/ci.yaml \
  .github/workflows/prod-operation.yaml \
  .github/workflows/restore-stranded-prod.yaml \
  .github/workflows/publish-release-recovery.yaml \
  .github/workflows/replace-openbao-server.yaml; do
  [[ -f "${path}" ]] || unknown "missing deploy or recovery surface ${path}"
  surfaces+=("${path}")
done
shopt -s nullglob
dr_workflows=(.github/workflows/dr-*.yaml .github/workflows/dr-*.yml)
shopt -u nullglob
((${#dr_workflows[@]} > 0)) || unknown 'no disaster-recovery workflow (.github/workflows/dr-*) found'
surfaces+=("${dr_workflows[@]}")
[[ -d .github/actions/deploy-prod ]] || unknown 'missing the deploy-prod composite action'
deploy_action_files=()
while IFS= read -r -d '' file; do
  deploy_action_files+=("${file}")
done < <(find .github/actions/deploy-prod -type f -print0)
((${#deploy_action_files[@]} > 0)) || unknown 'the deploy-prod composite action has no files'
surfaces+=("${deploy_action_files[@]}")

escape_ere() {
  # shellcheck disable=SC2016 # a sed bracket expression, not a shell expansion
  printf '%s' "$1" | sed 's/[][\.*^$(){}?+|]/\\&/g'
}

# A hostname matches only as a whole label sequence, so hosts.<domain> or
# my-sts.<domain> are not mistaken for the broker.
boundary='(^|[^[:alnum:]_.-])'
pattern="octo-sts|${boundary}$(escape_ere "${literal_host}")|${boundary}$(escape_ere "${resolved_host}")"

status=0
findings="$(grep -n -i -E -H -e "${pattern}" -- "${surfaces[@]}")" || status=$?
case "${status}" in
  0)
    printf 'FAIL: a deploy or recovery surface depends on the GitHub token broker (ADR 0020):\n%s\n' "${findings}" >&2
    exit 1
    ;;
  1) ;;
  *) unknown "could not scan the deploy and recovery surfaces (grep exit ${status})" ;;
esac
printf 'PASS: %d deploy and recovery files never reach octo-sts or %s\n' "${#surfaces[@]}" "${resolved_host}"
