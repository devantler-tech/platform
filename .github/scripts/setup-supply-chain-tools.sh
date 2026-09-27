#!/usr/bin/env bash
set -euo pipefail

# These pins authorize the bytes used to sign production artifacts and generate
# their SBOMs. Update each version AND its SHA-256 together in the same PR,
# using the named release's published checksums. A version-only Renovate bump
# fails closed; a downloaded checksum never replaces a reviewed local pin.
# renovate: datasource=github-releases depName=sigstore/cosign extractVersion=^v(?<version>.+)$
COSIGN_VERSION="3.1.3"
# cosign_checksums.txt: cosign-linux-amd64
COSIGN_SHA256="4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71"
# renovate: datasource=github-releases depName=anchore/syft extractVersion=^v(?<version>.+)$
SYFT_VERSION="1.52.0"
# syft_1.52.0_checksums.txt: syft_1.52.0_linux_amd64.tar.gz
SYFT_SHA256="caeedb81fb0491615f1ebd1761e4145d41ee86dd2cc7bf80669f9f5ad9d6133d"

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
# because one momentary error from the release host would otherwise fail the job. Every byte
# is still checked against the reviewed pin below, and a persistent failure still stops here.
download() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --retry 5 --retry-delay 3 "$1" --output "$2"
}

download "https://github.com/sigstore/cosign/releases/download/v${COSIGN_VERSION}/cosign-linux-amd64" \
  "${download_dir}/cosign"
verify_digest cosign "${COSIGN_SHA256}" "${download_dir}/cosign"

download "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/syft_${SYFT_VERSION}_linux_amd64.tar.gz" \
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
