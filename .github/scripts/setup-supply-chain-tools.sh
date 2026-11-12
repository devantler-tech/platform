#!/usr/bin/env bash
set -euo pipefail

# These pins authorize the bytes used to sign production artifacts and generate
# their SBOMs. Update each version AND its SHA-256 together in the same PR,
# using the named release's published checksums. A version-only Renovate bump
# fails closed; a downloaded checksum never replaces a reviewed local pin.
# renovate: datasource=github-releases depName=sigstore/cosign extractVersion=^v(?<version>.+)$
COSIGN_VERSION="3.1.4"
# cosign_checksums.txt: cosign-linux-amd64
COSIGN_SHA256="424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa"
# renovate: datasource=github-releases depName=anchore/syft extractVersion=^v(?<version>.+)$
SYFT_VERSION="1.56.0"
# syft_1.56.0_checksums.txt: syft_1.56.0_linux_amd64.tar.gz
SYFT_SHA256="1c2493cfcf52d3367d6cb25780d9190c111c2219c34c820574c43e8bae15c528"
# renovate: datasource=github-releases depName=aquasecurity/trivy extractVersion=^v(?<version>.+)$
TRIVY_VERSION="0.76.2"
# trivy_0.76.1_checksums.txt: trivy_0.76.1_Linux-64bit.tar.gz
TRIVY_SHA256="c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0"

if (($# > 1)) || [[ $# == 1 && $1 != trivy-boundary ]]; then
  echo "Usage: $0 [trivy-boundary]" >&2
  exit 2
fi

if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then
  echo '::error::Supply-chain tool pins support Linux x86_64 runners. Add reviewed release digests before using another platform.' >&2
  exit 1
fi
: "${GITHUB_PATH:?GITHUB_PATH must name the runner path file}"

download_dir=$(mktemp -d "${RUNNER_TEMP:-/tmp}/supply-chain-tools.XXXXXX")
trap 'rm -rf "${download_dir}"' EXIT

verify_digest() {
  local tool=$1 expected=$2 file=$3
  if ! printf '%s  %s\n' "${expected}" "${file}" | sha256sum -c - >/dev/null; then
    echo "::error::${tool} digest mismatch. Refusing to install or execute downloaded tools. Review the release checksums and update its version and SHA-256 together in .github/scripts/setup-supply-chain-tools.sh." >&2
    exit 1
  fi
}

# Release downloads retry curl's transient failures (timeouts, 408, 429 and 5xx) a few times,
# because one momentary error from the release host would otherwise fail the job. With
# GITHUB_TOKEN set they use the authenticated asset API first and fall back to the public
# URL (#5942). Every byte is still checked against the reviewed pin below, and a
# persistent failure still stops here.
# shellcheck source=.github/scripts/release-asset-download.sh
source "$(dirname "${BASH_SOURCE[0]}")/release-asset-download.sh"

if [[ ${1:-} == trivy-boundary ]]; then
  : "${RUNNER_TEMP:?RUNNER_TEMP must name the runner temporary directory}"
  trivy_dir="${RUNNER_TEMP}/platform-validation-tools"
  release_asset_download aquasecurity/trivy "v${TRIVY_VERSION}" "trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz" \
    "${download_dir}/trivy.tar.gz"
  verify_digest trivy "${TRIVY_SHA256}" "${download_dir}/trivy.tar.gz"
  tar -xzf "${download_dir}/trivy.tar.gz" -C "${download_dir}" trivy
  mkdir -p "${trivy_dir}"
  install -m 0755 "${download_dir}/trivy" "${trivy_dir}/trivy"
  "${trivy_dir}/trivy" --version
  printf '%s\n' "${trivy_dir}" >>"${GITHUB_PATH}"
  exit 0
fi

release_asset_download sigstore/cosign "v${COSIGN_VERSION}" cosign-linux-amd64 \
  "${download_dir}/cosign"
verify_digest cosign "${COSIGN_SHA256}" "${download_dir}/cosign"

release_asset_download anchore/syft "v${SYFT_VERSION}" "syft_${SYFT_VERSION}_linux_amd64.tar.gz" \
  "${download_dir}/syft.tar.gz"
verify_digest syft "${SYFT_SHA256}" "${download_dir}/syft.tar.gz"

# Verify both downloads before extracting or installing either tool. Extract
# only the executable into the private directory, never the whole archive.
tar -xzf "${download_dir}/syft.tar.gz" -C "${download_dir}" syft
sudo install -m 0755 "${download_dir}/cosign" /usr/local/bin/cosign
sudo install -m 0755 "${download_dir}/syft" /usr/local/bin/syft
printf '%s\n' /usr/local/bin >>"${GITHUB_PATH}"
/usr/local/bin/cosign version
/usr/local/bin/syft version
