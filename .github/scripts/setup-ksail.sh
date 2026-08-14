#!/usr/bin/env bash
set -euo pipefail

if [ -z "${KSAIL_VERSION:-}" ]; then
  echo "::error::KSAIL_VERSION must be set"
  exit 1
fi

asset_name="ksail_${KSAIL_VERSION}_linux_amd64.tar.gz"
release_base="https://github.com/devantler-tech/ksail/releases/download/v${KSAIL_VERSION}"
tarball="${RUNNER_TEMP:-/tmp}/${asset_name}"
checksums="${RUNNER_TEMP:-/tmp}/ksail_${KSAIL_VERSION}_checksums.txt"

curl -fsSL "${release_base}/${asset_name}" -o "${tarball}"
curl -fsSL "${release_base}/ksail_${KSAIL_VERSION}_checksums.txt" -o "${checksums}"

expected_digest=$(awk -v asset="${asset_name}" '$2 == asset {print $1}' "${checksums}")
if [ -z "${expected_digest}" ]; then
  echo "::error::no published checksum for ${asset_name} — refusing to install unverified"
  exit 1
fi

actual_digest=$(sha256sum "${tarball}" | cut -d' ' -f1)
if [ "${expected_digest}" != "${actual_digest}" ]; then
  echo "::error::checksum mismatch for ${asset_name}: expected ${expected_digest}, got ${actual_digest}"
  exit 1
fi

tar -xzf "${tarball}" -C "${RUNNER_TEMP:-/tmp}" ksail
sudo install "${RUNNER_TEMP:-/tmp}/ksail" /usr/local/bin/ksail
ksail --version
