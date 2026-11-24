#!/usr/bin/env bash
# Regression and incomplete-census controls for
# scripts/guard-github-broker-independence.sh (devantler-tech/monorepo ADR 0020).
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
guard="${root_dir}/scripts/guard-github-broker-independence.sh"
readonly guard
work="$(mktemp -d)"
readonly work
trap 'rm -rf "${work}"' EXIT

# The tree as committed passes.
bash "${guard}" "${root_dir}"

# A copy of every input the guard reads, to mutate one case at a time.
fixture() {
  rm -rf "${work}/repo"
  mkdir -p "${work}/repo/.github" "${work}/repo/k8s/providers/hetzner/apps" "${work}/repo/k8s/clusters/prod/bootstrap"
  cp -R "${root_dir}/.github/workflows" "${root_dir}/.github/actions" "${work}/repo/.github/"
  cp -R "${root_dir}/k8s/providers/hetzner/apps/github-broker" "${work}/repo/k8s/providers/hetzner/apps/"
  cp "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml" "${work}/repo/k8s/clusters/prod/bootstrap/"
}

expect() {
  local want="$1" message="$2" status=0
  bash "${guard}" "${work}/repo" >"${work}/result" 2>&1 || status=$?
  if [[ "${status}" != "${want}" ]]; then
    printf 'FAIL: %s: expected exit %s, got %s\n' "${message}" "${want}" "${status}" >&2
    cat "${work}/result" >&2
    exit 1
  fi
}

append() {
  printf '%s\n' "$2" >>"${work}/repo/$1"
}

fixture
expect 0 'an unchanged copy passes'

# Each way a deploy or recovery path could start depending on the broker fails.
domain="$(yq '.data.domain' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml")"
dr_workflow="$(cd "${work}/repo" && find .github/workflows -name 'dr-*' -type f | head -n 1)"
for case in \
  ".github/workflows/cd.yaml|      - uses: octo-sts/action@0000000000000000000000000000000000000000" \
  ".github/workflows/ci.yaml|          domain: sts.${domain}" \
  ".github/workflows/prod-operation.yaml|        run: curl -fsS https://STS.${domain}/sts/exchange" \
  "${dr_workflow}|          image: ghcr.io/octo-sts/app:0.14.4" \
  ".github/actions/deploy-prod/action.yml|# reaches sts.\${domain} for a token" \
  ".github/workflows/restore-stranded-prod.yaml|      url: \"sts.${domain}\""; do
  fixture
  append "${case%%|*}" "${case#*|}"
  expect 1 "a broker reference in ${case%%|*}"
  grep -q 'depends on the GitHub token broker' "${work}/result"
done

# Hostnames that only end in the broker's labels are not the broker.
fixture
append .github/workflows/cd.yaml "# hosts.${domain} and my-sts.${domain} are other services"
expect 0 'a longer hostname is not the broker'

# An incomplete census is UNKNOWN, never a pass.
fixture
rm "${work}/repo/.github/workflows/prod-operation.yaml"
expect 2 'a missing recovery workflow'
fixture
find "${work}/repo/.github/workflows" -name 'dr-*' -delete
expect 2 'no disaster-recovery workflow'
fixture
rm -rf "${work}/repo/.github/actions/deploy-prod"
expect 2 'a missing deploy action'
fixture
# shellcheck disable=SC2016 # a literal Flux variable
yq -i '.spec.hostnames += ["other.${domain}"]' "${work}/repo/k8s/providers/hetzner/apps/github-broker/http-route.yaml"
expect 2 'two broker hostnames'
fixture
yq -i '.spec.hostnames = ["sts.example.com"]' "${work}/repo/k8s/providers/hetzner/apps/github-broker/http-route.yaml"
expect 2 'a hostname outside the domain variable'
fixture
rm "${work}/repo/k8s/providers/hetzner/apps/github-broker/http-route.yaml"
expect 2 'a missing broker route'

printf 'PASS: broker independence regression and incomplete-census controls\n'
