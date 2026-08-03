#!/usr/bin/env bash
# Refresh the pinned KubeVirt and CDI release bundles without losing the
# repository's resource-scoped Checkov dispositions.
#
# Bump each version and SHA-256 constant below, then run this script from
# anywhere in the repository. A renamed target or a new Checkov finding is a
# hard failure: the updater never replaces the committed bundle until checksum,
# annotation, and scan validation all succeed.
set -euo pipefail

readonly cdi_version='v1.65.0'
readonly cdi_sha256='e96d59abdf358c5161cb96adcfdcc6107efc3fb608ec93ade11578c94a222015'
readonly kubevirt_version='v1.8.0'
readonly kubevirt_sha256='e9e92c15bca0531bf0b7db2c2dfc83b6b9bdbf1a6f3f96945f67d90d702193b5'

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly repo_root
work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

require_tool() {
  local tool="$1"
  if ! command -v "${tool}" >/dev/null 2>&1; then
    printf '%s is required to update vendored operators\n' "${tool}" >&2
    exit 2
  fi
}

prepare_bundle() {
  local bundle="$1"
  local url="$2"
  local expected_sha256="$3"
  local downloaded="${work_dir}/${bundle}-upstream.yaml"
  local annotated="${work_dir}/${bundle}-annotated.yaml"

  curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
    --retry 3 --retry-all-errors "${url}" --output "${downloaded}"
  printf '%s  %s\n' "${expected_sha256}" "${downloaded}" | sha256sum -c -

  (
    cd "${repo_root}"
    go run ./scripts/annotate-vendored-checkov --bundle "${bundle}" \
      <"${downloaded}" >"${annotated}"
  )

  # The known, reviewed dispositions must clear the current upstream bundle,
  # while any new check introduced by a vendor bump remains visible and blocks
  # replacement until it receives an explicit disposition.
  checkov --file "${annotated}" --framework kubernetes --compact --quiet
}

require_tool curl
require_tool go
require_tool checkov
require_tool sha256sum

prepare_bundle \
  cdi \
  "https://github.com/kubevirt/containerized-data-importer/releases/download/${cdi_version}/cdi-operator.yaml" \
  "${cdi_sha256}"

prepare_bundle \
  kubevirt \
  "https://github.com/kubevirt/kubevirt/releases/download/${kubevirt_version}/kubevirt-operator.yaml" \
  "${kubevirt_sha256}"

# Commit both prepared outputs together only after both upstream bundles pass.
# A failed second download or scan therefore cannot leave a half-updated pair.
mv \
  "${work_dir}/cdi-annotated.yaml" \
  "${repo_root}/k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml"
mv \
  "${work_dir}/kubevirt-annotated.yaml" \
  "${repo_root}/k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml"

printf 'Updated CDI %s and KubeVirt %s; both annotated bundles pass Checkov.\n' \
  "${cdi_version}" "${kubevirt_version}"
