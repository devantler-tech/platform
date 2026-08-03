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
# Keep this aligned with CI_CHECKOV_VERSION in megalinter-scan-counts.sh so a
# local vendor refresh cannot miss a rule that the non-blocking CI scan knows.
readonly checkov_version='3.3.2'
# CKV2_K8S_6 only understands networking.k8s.io NetworkPolicy. An isolated
# bundle scan cannot see that cilium-network-policy.yaml protects every CDI
# endpoint; the full-repository CI scan remains unskipped and owns graph checks.
readonly isolated_scan_skip_check='CKV2_K8S_6'

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
  local findings="${work_dir}/${bundle}-findings.json"
  local annotated="${work_dir}/${bundle}-annotated.yaml"
  local annotated_report="${work_dir}/${bundle}-annotated-report.json"
  local checkov_rc=0

  curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
    --retry 3 --retry-all-errors "${url}" --output "${downloaded}"
  printf '%s  %s\n' "${expected_sha256}" "${downloaded}" | sha256sum -c -

  # Prove that every reviewed disposition is still necessary on this exact
  # upstream asset. An upstream security fix makes the update stop here until
  # the obsolete annotation is removed from the configured target list.
  checkov --file "${downloaded}" --framework kubernetes secrets \
    --skip-check "${isolated_scan_skip_check}" \
    --output json --quiet >"${findings}" || checkov_rc=$?
  if [ "${checkov_rc}" -gt 1 ]; then
    printf 'checkov could not scan the unannotated %s bundle (exit %d)\n' \
      "${bundle}" "${checkov_rc}" >&2
    exit 2
  fi

  (
    cd "${repo_root}"
    go run ./scripts/annotate-vendored-checkov --bundle "${bundle}" \
      --validate-findings <"${findings}"
    go run ./scripts/annotate-vendored-checkov --bundle "${bundle}" \
      <"${downloaded}" >"${annotated}"
  )

  # The known, reviewed dispositions must clear the current upstream bundle,
  # while any new check introduced by a vendor bump remains visible and blocks
  # replacement until it receives an explicit disposition.
  checkov_rc=0
  checkov --file "${annotated}" --framework kubernetes secrets \
    --skip-check "${isolated_scan_skip_check}" \
    --output json --quiet >"${annotated_report}" || checkov_rc=$?
  (
    cd "${repo_root}"
    go run ./scripts/annotate-vendored-checkov --bundle "${bundle}" \
      --validate-report <"${annotated_report}"
  )
  if [ "${checkov_rc}" -ne 0 ]; then
    printf 'checkov found an undispositioned issue in the annotated %s bundle (exit %d):\n' \
      "${bundle}" "${checkov_rc}" >&2
    while IFS= read -r line; do
      printf '%s\n' "${line}" >&2
    done <"${annotated_report}"
    exit 2
  fi
}

require_tool curl
require_tool go
require_tool checkov
require_tool sha256sum

actual_checkov_version="$(checkov --version)"
readonly actual_checkov_version
if [ "${actual_checkov_version}" != "${checkov_version}" ]; then
  printf 'checkov %s is required; found %s\n' \
    "${checkov_version}" "${actual_checkov_version}" >&2
  exit 2
fi

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
