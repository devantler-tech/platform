#!/usr/bin/env bash
set -euo pipefail

if [ -z "${KSAIL_VERSION:-}" ]; then
  echo "::error::KSAIL_VERSION must be set"
  exit 1
fi

asset_name="ksail_${KSAIL_VERSION}_linux_amd64.tar.gz"
release_base="https://github.com/devantler-tech/ksail/releases/download/v${KSAIL_VERSION}"
api_url="https://api.github.com/repos/devantler-tech/ksail/releases/tags/v${KSAIL_VERSION}"
tarball="${RUNNER_TEMP:-/tmp}/${asset_name}"
checksums="${RUNNER_TEMP:-/tmp}/ksail_${KSAIL_VERSION}_checksums.txt"
release_json="${RUNNER_TEMP:-/tmp}/ksail-release-${KSAIL_VERSION}.json"

curl_headers=(-H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28")
if [ -n "${GITHUB_TOKEN:-}" ]; then
  curl_headers+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

curl -fsSL "${release_base}/${asset_name}" -o "${tarball}"
curl -fsSL "${release_base}/ksail_${KSAIL_VERSION}_checksums.txt" -o "${checksums}"
curl -fsSL "${curl_headers[@]}" "${api_url}" -o "${release_json}"

manifest_digest=$(awk -v asset="${asset_name}" '$2 == asset {print $1}' "${checksums}")
if [ -z "${manifest_digest}" ]; then
  echo "::error::no published checksum for ${asset_name} — refusing to install unverified"
  exit 1
fi

metadata_digest=$(
  jq -r --arg asset "${asset_name}" \
    '[.assets[] | select(.name == $asset) | .digest | select(type == "string" and startswith("sha256:"))][0] // empty | sub("^sha256:"; "")' \
    "${release_json}"
)
if [ -z "${metadata_digest}" ]; then
  echo "::error::no sha256 digest for ${asset_name} in GitHub release metadata — refusing to install unverified"
  exit 1
fi

actual_digest=$(sha256sum "${tarball}" | cut -d' ' -f1)
if [ "${metadata_digest}" != "${actual_digest}" ]; then
  echo "::error::release metadata digest mismatch for ${asset_name}: expected ${metadata_digest}, got ${actual_digest}"
  exit 1
fi

if [ "${manifest_digest}" != "${actual_digest}" ]; then
  echo "::error::checksum manifest mismatch for ${asset_name}: expected ${manifest_digest}, got ${actual_digest}"
  exit 1
fi

tar -xzf "${tarball}" -C "${RUNNER_TEMP:-/tmp}" ksail
sudo install "${RUNNER_TEMP:-/tmp}/ksail" /usr/local/bin/ksail
ksail --version
